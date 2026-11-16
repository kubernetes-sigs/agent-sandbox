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

package sandbox

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// fakeLimitsService records the ExecuteWithLimits request and answers with
// response, or err.
type fakeLimitsService struct {
	processv1.UnimplementedProcessServiceServer
	lastRequest *processv1.ExecuteWithLimitsRequest
	response    *processv1.ExecuteWithLimitsResponse
	err         error
}

func (f *fakeLimitsService) ExecuteWithLimits(_ context.Context, req *processv1.ExecuteWithLimitsRequest) (*processv1.ExecuteWithLimitsResponse, error) {
	f.lastRequest = req
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

func TestRunWithLimits_SendsLimitsAndMapsTheResult(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://unused.invalid")
	svc := &fakeLimitsService{response: &processv1.ExecuteWithLimitsResponse{
		ExitCode:          -1,
		Stdout:            []byte("partial"),
		Stderr:            []byte("err"),
		TerminationReason: processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED,
		ElapsedDuration:   durationpb.New(1500 * time.Millisecond),
		StdoutTruncated:   true,
	}}
	startFakeProcessServer(t, sb, svc)

	result, err := sb.RunWithLimits(context.Background(), "make test",
		RunLimits{TimeLimit: 30 * time.Second, StdoutLimitBytes: 1 << 20, StderrLimitBytes: 4096},
		WithEnv(map[string]string{"CI": "1"}), WithWorkingDir("proj"))
	require.NoError(t, err)

	req := svc.lastRequest
	require.Equal(t, []string{"/bin/sh", "-c", "make test"}, req.GetConfig().GetCommand())
	require.Equal(t, map[string]string{"CI": "1"}, req.GetConfig().GetEnvVars())
	require.Equal(t, "proj", req.GetConfig().GetCwd())
	require.Equal(t, 30*time.Second, req.GetTimeLimit().AsDuration())
	require.Equal(t, uint64(1<<20), req.GetStdoutLimitBytes())
	require.Equal(t, uint64(4096), req.GetStderrLimitBytes())

	require.Equal(t, "partial", result.Stdout)
	require.Equal(t, "err", result.Stderr)
	require.Equal(t, -1, result.ExitCode)
	require.Equal(t, TerminationTimeLimitExceeded, result.TerminationReason)
	require.True(t, result.TimedOut())
	require.Equal(t, 1500*time.Millisecond, result.Elapsed)
	require.True(t, result.StdoutTruncated)
	require.False(t, result.StderrTruncated)
}

func TestRunWithLimits_ZeroLimitsAreLeftToPolicy(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://unused.invalid")
	svc := &fakeLimitsService{response: &processv1.ExecuteWithLimitsResponse{
		TerminationReason: processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_EXITED,
	}}
	startFakeProcessServer(t, sb, svc)

	result, err := sb.RunWithLimits(context.Background(), "true", RunLimits{})
	require.NoError(t, err)
	require.Nil(t, svc.lastRequest.TimeLimit)
	require.Nil(t, svc.lastRequest.StdoutLimitBytes)
	require.Nil(t, svc.lastRequest.StderrLimitBytes)
	require.Equal(t, TerminationExited, result.TerminationReason)
	require.False(t, result.TimedOut())
}

func TestRunWithLimits_OldSandboxdIsUnsupported(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://unused.invalid")
	startFakeProcessServer(t, sb, &fakeLimitsService{err: status.Error(codes.Unimplemented, "unknown method ExecuteWithLimits")})

	_, err := sb.RunWithLimits(context.Background(), "true", RunLimits{})
	require.ErrorIs(t, err, ErrUnsupportedByRuntime)
}

func TestRunWithLimits_RefusesBadInput(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://unused.invalid")
	svc := &fakeLimitsService{}
	startFakeProcessServer(t, sb, svc)

	_, err := sb.RunWithLimits(context.Background(), "true", RunLimits{TimeLimit: -time.Second})
	require.Error(t, err)
	require.Nil(t, svc.lastRequest, "a refused call sends nothing")

	legacy := newReadySandboxdTestSandboxWith("http://unused.invalid", func(o *Options) { o.Runtime = RuntimeLegacyPython })
	_, err = legacy.RunWithLimits(context.Background(), "true", RunLimits{})
	require.ErrorIs(t, err, ErrUnsupportedByRuntime)
}

// Explicit limits can allow more retained output than gRPC's default 4 MiB
// receive limit; the response must still arrive, since the command already ran.
func TestRunWithLimits_ReceivesOutputAboveTheDefaultMessageLimit(t *testing.T) {
	sb := newReadySandboxdTestSandbox("http://unused.invalid")
	stdout := bytes.Repeat([]byte("o"), 3<<20)
	stderr := bytes.Repeat([]byte("e"), 3<<20)
	startFakeProcessServer(t, sb, &fakeLimitsService{response: &processv1.ExecuteWithLimitsResponse{
		Stdout:            stdout,
		Stderr:            stderr,
		TerminationReason: processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_EXITED,
	}})

	result, err := sb.RunWithLimits(context.Background(), "make test",
		RunLimits{StdoutLimitBytes: 3 << 20, StderrLimitBytes: 3 << 20})
	require.NoError(t, err)
	require.Len(t, result.Stdout, 3<<20)
	require.Len(t, result.Stderr, 3<<20)
}

func TestReceiveLimitFor(t *testing.T) {
	require.Empty(t, receiveLimitFor(RunLimits{}), "no explicit limits keep the default")
	require.Empty(t, receiveLimitFor(RunLimits{StdoutLimitBytes: 1 << 20, StderrLimitBytes: 1 << 20}), "small limits never lower it")
	require.Len(t, receiveLimitFor(RunLimits{StdoutLimitBytes: 3 << 20, StderrLimitBytes: 3 << 20}), 1)
	require.Len(t, receiveLimitFor(RunLimits{StdoutLimitBytes: math.MaxUint64, StderrLimitBytes: math.MaxUint64}), 1, "huge limits saturate instead of wrapping")
}

func TestTerminationReasonKeepsUnknownValues(t *testing.T) {
	require.Equal(t, TerminationReason("unspecified"), terminationReason(processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_UNSPECIFIED))
	require.Equal(t, TerminationReason("99"), terminationReason(processv1.ExecuteTerminationReason(99)))
}

// The real sandboxd: the time limit ends the command and truncation keeps a
// prefix, both reported in the result rather than as errors.
func TestRunWithLimits_RealSandboxd(t *testing.T) {
	sb, _ := newRealProcessSandbox(t)
	ctx := processTestContext(t)

	result, err := sb.RunWithLimits(ctx, "echo started; sleep 30", RunLimits{TimeLimit: 300 * time.Millisecond})
	require.NoError(t, err)
	require.True(t, result.TimedOut(), "termination = %q", result.TerminationReason)
	require.Equal(t, "started\n", result.Stdout)
	require.Less(t, result.Elapsed, 5*time.Second)

	result, err = sb.RunWithLimits(ctx, "printf '%0100d' 0; printf oops >&2", RunLimits{StdoutLimitBytes: 10})
	require.NoError(t, err)
	require.Equal(t, TerminationExited, result.TerminationReason)
	require.Equal(t, 0, result.ExitCode)
	require.Equal(t, strings.Repeat("0", 10), result.Stdout)
	require.True(t, result.StdoutTruncated)
	require.Equal(t, "oops", result.Stderr)
	require.False(t, result.StderrTruncated)
}
