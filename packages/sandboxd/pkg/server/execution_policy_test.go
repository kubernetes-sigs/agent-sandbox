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

package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

func newPolicyClient(t *testing.T, policy ExecutionPolicy) processv1.ProcessServiceClient {
	t.Helper()
	srv, err := New(Options{RootDir: t.TempDir(), ExecutionPolicy: policy})
	require.NoError(t, err)
	g := grpc.NewServer()
	srv.RegisterGRPC(g)
	return connectProcessClient(t, g)
}

func TestExecutionPolicyDefaultOutput(t *testing.T) {
	client := newPolicyClient(t, ExecutionPolicy{DefaultStdoutLimitBytes: 3, MaxStdoutLimitBytes: 5})
	resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config: &processv1.ProcessConfig{Command: []string{"printf", "abcdef"}},
	})
	require.NoError(t, err)
	require.Equal(t, "abc", string(resp.GetStdout()))
	require.True(t, resp.GetStdoutTruncated())
}

func TestExecutionPolicyOutputResolution(t *testing.T) {
	for _, tc := range []struct {
		name             string
		policy           ExecutionPolicy
		stdout, stderr   *uint64
		wantOut, wantErr string
	}{
		{"request overrides default", ExecutionPolicy{DefaultStdoutLimitBytes: 3, MaxStdoutLimitBytes: 5}, proto.Uint64(4), nil, "abcd", "uvwxyz"},
		{"request equals maximum", ExecutionPolicy{DefaultStdoutLimitBytes: 3, MaxStdoutLimitBytes: 5}, proto.Uint64(5), nil, "abcde", "uvwxyz"},
		{"maximum as fallback", ExecutionPolicy{MaxStdoutLimitBytes: 4, MaxStderrLimitBytes: 2}, nil, nil, "abcd", "uv"},
		{"defaults for both streams", ExecutionPolicy{DefaultStdoutLimitBytes: 3, DefaultStderrLimitBytes: 2}, nil, nil, "abc", "uv"},
		{"stderr request overrides default", ExecutionPolicy{DefaultStderrLimitBytes: 2, MaxStderrLimitBytes: 5}, nil, proto.Uint64(4), "abcdef", "uvwx"},
		{"unconfigured", ExecutionPolicy{}, nil, nil, "abcdef", "uvwxyz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newPolicyClient(t, tc.policy)
			resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
				Config:           &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf abcdef; printf uvwxyz >&2"}},
				StdoutLimitBytes: tc.stdout, StderrLimitBytes: tc.stderr,
			})
			require.NoError(t, err)
			require.Equal(t, tc.wantOut, string(resp.GetStdout()))
			require.Equal(t, tc.wantErr, string(resp.GetStderr()))
			require.Equal(t, len(tc.wantOut) < 6, resp.GetStdoutTruncated())
			require.Equal(t, len(tc.wantErr) < 6, resp.GetStderrTruncated())
		})
	}
}

func TestExecutionPolicyTimeResolution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		policy    ExecutionPolicy
		requested *durationpb.Duration
		minimum   time.Duration
	}{
		{"default", ExecutionPolicy{DefaultTimeLimit: 100 * time.Millisecond, MaxTimeLimit: time.Second}, nil, 100 * time.Millisecond},
		{"maximum as fallback", ExecutionPolicy{MaxTimeLimit: 100 * time.Millisecond}, nil, 100 * time.Millisecond},
		{"request overrides default", ExecutionPolicy{DefaultTimeLimit: time.Second, MaxTimeLimit: 2 * time.Second}, durationpb.New(100 * time.Millisecond), 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newPolicyClient(t, tc.policy)
			resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
				Config:    &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf started; exec sleep 30"}},
				TimeLimit: tc.requested,
			})
			require.NoError(t, err)
			require.Equal(t, processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED, resp.GetTerminationReason())
			require.Equal(t, "started", string(resp.GetStdout()))
			require.GreaterOrEqual(t, resp.GetElapsedDuration().AsDuration(), tc.minimum)
		})
	}
}

func TestExecutionPolicyRejectsBeforeExecution(t *testing.T) {
	client := newPolicyClient(t, ExecutionPolicy{MaxTimeLimit: time.Second, MaxStdoutLimitBytes: 5, MaxStderrLimitBytes: 5})
	for _, req := range []*processv1.ExecuteWithLimitsRequest{
		{TimeLimit: durationpb.New(2 * time.Second)},
		{StdoutLimitBytes: proto.Uint64(6)},
		{StderrLimitBytes: proto.Uint64(6)},
	} {
		req.Config = &processv1.ProcessConfig{Command: []string{"touch", "over-limit-ran"}}
		_, err := client.ExecuteWithLimits(testCtx(t), req)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	resp, err := client.ExecuteWithLimits(testCtx(t), &processv1.ExecuteWithLimitsRequest{
		Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "test ! -e over-limit-ran"}},
	})
	require.NoError(t, err)
	require.Zero(t, resp.GetExitCode())
}

func TestExecutionPolicyLegacyExecute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   ExecutionPolicy
		command  string
		wantCode codes.Code
		wantExit int32
		wantOut  string
	}{
		{"unconfigured", ExecutionPolicy{}, "printf abcdef", codes.OK, 0, "abcdef"},
		{"normal nonzero", ExecutionPolicy{DefaultStdoutLimitBytes: 4, MaxTimeLimit: time.Second}, "printf abc; exit 7", codes.OK, 7, "abc"},
		{"stdout truncation", ExecutionPolicy{DefaultStdoutLimitBytes: 3}, "printf abcdef; touch completed", codes.ResourceExhausted, 0, ""},
		{"stderr truncation", ExecutionPolicy{MaxStderrLimitBytes: 3}, "printf abcdef >&2; touch completed", codes.ResourceExhausted, 0, ""},
		{"time limit", ExecutionPolicy{MaxTimeLimit: 100 * time.Millisecond}, "exec sleep 30", codes.ResourceExhausted, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newPolicyClient(t, tc.policy)
			resp, err := client.Execute(testCtx(t), &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", tc.command}}})
			require.Equal(t, tc.wantCode, status.Code(err))
			if tc.wantCode == codes.OK {
				require.Equal(t, tc.wantExit, resp.GetExitCode())
				require.Equal(t, tc.wantOut, string(resp.GetStdout()))
			} else {
				require.Nil(t, resp)
			}
			if tc.name == "stdout truncation" || tc.name == "stderr truncation" {
				resp, err = client.Execute(testCtx(t), &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "test -e completed"}}})
				require.NoError(t, err)
				require.Zero(t, resp.GetExitCode(), "truncation must not terminate the command")
			}
		})
	}
}

func TestExecutionPolicyLeavesStartUnchanged(t *testing.T) {
	client := newPolicyClient(t, ExecutionPolicy{MaxTimeLimit: 50 * time.Millisecond, MaxStdoutLimitBytes: 2})
	stream, err := client.Start(testCtx(t), &processv1.StartRequest{Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", "printf abcdef; sleep 0.15; printf XYZ"}}})
	require.NoError(t, err)
	resp := drainStart(t, stream)
	require.Zero(t, resp.exitCode)
	require.Equal(t, "abcdefXYZ", resp.stdout.String())
}

func TestExecutionPolicyInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy ExecutionPolicy
	}{
		{"negative default", ExecutionPolicy{DefaultTimeLimit: -time.Second}},
		{"negative maximum", ExecutionPolicy{MaxTimeLimit: -time.Second}},
		{"time default above maximum", ExecutionPolicy{DefaultTimeLimit: 2 * time.Second, MaxTimeLimit: time.Second}},
		{"stdout default above maximum", ExecutionPolicy{DefaultStdoutLimitBytes: 5, MaxStdoutLimitBytes: 3}},
		{"stderr default above maximum", ExecutionPolicy{DefaultStderrLimitBytes: 5, MaxStderrLimitBytes: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(Options{RootDir: t.TempDir(), ExecutionPolicy: tc.policy})
			require.Nil(t, srv)
			require.ErrorContains(t, err, "invalid execution policy")
		})
	}
}
