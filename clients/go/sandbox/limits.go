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
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

// RunLimits bounds one command run by RunWithLimits. A zero field is left
// out of the request, so sandboxd applies its deployment policy for it.
type RunLimits struct {
	// TimeLimit bounds the command's execution, including launch and output
	// collection. sandboxd ends the command when it is reached and reports
	// TerminationTimeLimitExceeded. It is independent of the call's deadline.
	TimeLimit time.Duration
	// StdoutLimitBytes and StderrLimitBytes cap how much of each stream is
	// kept. Further output is drained and discarded, and the result reports
	// the stream as truncated. The call accepts a response as large as the
	// explicit limits allow; a stream left to sandboxd's policy (which may be
	// unlimited) is received within gRPC's default 4 MiB, as with Run, so set
	// both limits when more than 4 MiB of output may come back.
	StdoutLimitBytes uint64
	StderrLimitBytes uint64
}

// TerminationReason is how a command run by RunWithLimits ended.
type TerminationReason string

const (
	// TerminationExited: the program exited on its own, or from a signal
	// other than an execution limit. ExitCode says how.
	TerminationExited TerminationReason = "exited"
	// TerminationTimeLimitExceeded: sandboxd ended the command at TimeLimit.
	TerminationTimeLimitExceeded TerminationReason = "time_limit_exceeded"
)

// LimitedExecutionResult is the outcome of RunWithLimits.
type LimitedExecutionResult struct {
	ExecutionResult
	TerminationReason TerminationReason
	// Elapsed is the wall-clock time from launch preparation through
	// termination and I/O cleanup, as sandboxd measured it.
	Elapsed         time.Duration
	StdoutTruncated bool
	StderrTruncated bool
}

// TimedOut reports whether sandboxd ended the command at its TimeLimit.
func (r *LimitedExecutionResult) TimedOut() bool {
	return r.TerminationReason == TerminationTimeLimitExceeded
}

// RunWithLimits runs command like Run, through sandboxd's ExecuteWithLimits:
// output beyond a stream's limit is discarded instead of failing the call,
// and a command that reaches TimeLimit is ended and reported, not returned
// as an error.
//
// Without a deadline on ctx, the call gets RequestTimeout, plus TimeLimit
// when one is set, so the RPC outlives the command it bounds. When the
// explicit stream limits allow a response over gRPC's default 4 MiB receive
// limit, the call accepts that much.
//
// It needs RuntimeSandboxd and a gRPC connection; the legacy runtime, and a
// sandboxd too old to have ExecuteWithLimits, return ErrUnsupportedByRuntime.
// WithEnv and WithWorkingDir apply as for Run; it never retries.
func (c *Commands) RunWithLimits(ctx context.Context, command string, limits RunLimits, opts ...CallOption) (*LimitedExecutionResult, error) {
	defer c.trackOp()()
	co := resolveCallOpts(opts)
	ctx, callCancel, _ := applyCallOptsNoRetry(ctx, opts)
	defer callCancel()
	ctx = withLifecycleSpan(ctx, c.lifecycleCtx())
	ctx, span := startSpan(ctx, c.tracer, c.svcName, "run_with_limits", AttrCommandExecutable.String(commandExecutable(command)))
	defer func() { span.End() }()

	fail := func(err error) (*LimitedExecutionResult, error) {
		recordError(span, err)
		return nil, err
	}
	if c.runtime != RuntimeSandboxd {
		return fail(fmt.Errorf("%s: run with limits: %w: the legacy python-runtime has no ExecuteWithLimits", c.errPrefix(), ErrUnsupportedByRuntime))
	}
	if limits.TimeLimit < 0 {
		return fail(fmt.Errorf("%s: run with limits: TimeLimit must not be negative", c.errPrefix()))
	}
	config, err := commandConfig(command, co)
	if err != nil {
		return fail(fmt.Errorf("%s: run with limits: %w", c.errPrefix(), err))
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var reqCancel context.CancelFunc
		ctx, reqCancel = context.WithTimeout(ctx, c.connector.requestTimeout+limits.TimeLimit)
		defer reqCancel()
	}

	conn, err := c.connector.GRPCConn()
	if err != nil {
		return fail(fmt.Errorf("%s: run with limits failed: %w", c.errPrefix(), err))
	}
	resp, err := processv1.NewProcessServiceClient(conn).ExecuteWithLimits(ctx, executeWithLimitsRequest(config, limits), receiveLimitFor(limits)...)
	if err != nil {
		if st, ok := status.FromError(err); ok {
			if st.Code() == codes.Unimplemented {
				err = errors.Join(ErrUnsupportedByRuntime, err)
			}
			err = fmt.Errorf("%s: run with limits: process service returned %s: %s: %w", c.errPrefix(), st.Code(), st.Message(), err)
		} else {
			err = fmt.Errorf("%s: run with limits failed: %w", c.errPrefix(), err)
		}
		return fail(err)
	}

	result := &LimitedExecutionResult{
		ExecutionResult: ExecutionResult{
			Stdout:   string(resp.GetStdout()),
			Stderr:   string(resp.GetStderr()),
			ExitCode: int(resp.GetExitCode()),
		},
		TerminationReason: terminationReason(resp.GetTerminationReason()),
		Elapsed:           resp.GetElapsedDuration().AsDuration(),
		StdoutTruncated:   resp.GetStdoutTruncated(),
		StderrTruncated:   resp.GetStderrTruncated(),
	}
	span.SetAttributes(AttrExitCode.Int(result.ExitCode))
	c.log.V(1).Info("run with limits completed", "exitCode", result.ExitCode, "termination", result.TerminationReason,
		"stdoutTruncated", result.StdoutTruncated, "stderrTruncated", result.StderrTruncated)
	return result, nil
}

// grpcDefaultMaxRecvMsgSize is grpc-go's default limit on a received message.
const grpcDefaultMaxRecvMsgSize = 4 << 20

// executeResponseOverheadBytes covers the response's fields other than the
// retained output.
const executeResponseOverheadBytes = 1024

// receiveLimitFor raises the call's receive limit when the explicit stream
// limits allow a response larger than grpc-go's default, so retained output
// is not rejected after the command already ran. It never lowers the limit:
// a stream left to sandboxd's policy can still return up to the default.
func receiveLimitFor(limits RunLimits) []grpc.CallOption {
	const ceiling = math.MaxInt32
	need := uint64(executeResponseOverheadBytes)
	for _, n := range []uint64{limits.StdoutLimitBytes, limits.StderrLimitBytes} {
		need = min(need+min(n, ceiling), ceiling)
	}
	if need <= grpcDefaultMaxRecvMsgSize {
		return nil
	}
	return []grpc.CallOption{grpc.MaxCallRecvMsgSize(int(need))}
}

// executeWithLimitsRequest leaves a zero limit unset, so sandboxd's policy
// applies; the proto requires explicit limits to be positive.
func executeWithLimitsRequest(config *processv1.ProcessConfig, limits RunLimits) *processv1.ExecuteWithLimitsRequest {
	req := &processv1.ExecuteWithLimitsRequest{Config: config}
	if limits.TimeLimit > 0 {
		req.TimeLimit = durationpb.New(limits.TimeLimit)
	}
	if limits.StdoutLimitBytes > 0 {
		req.StdoutLimitBytes = &limits.StdoutLimitBytes
	}
	if limits.StderrLimitBytes > 0 {
		req.StderrLimitBytes = &limits.StderrLimitBytes
	}
	return req
}

// terminationReason maps sandboxd's enum to the SDK's names; a value this
// SDK doesn't know keeps the enum's name without its prefix, lower-cased.
func terminationReason(r processv1.ExecuteTerminationReason) TerminationReason {
	switch r {
	case processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_EXITED:
		return TerminationExited
	case processv1.ExecuteTerminationReason_EXECUTE_TERMINATION_REASON_TIME_LIMIT_EXCEEDED:
		return TerminationTimeLimitExceeded
	}
	return TerminationReason(strings.ToLower(strings.TrimPrefix(r.String(), "EXECUTE_TERMINATION_REASON_")))
}

// RunWithLimits runs a command with execution limits. See
// Commands.RunWithLimits.
func (s *Sandbox) RunWithLimits(ctx context.Context, command string, limits RunLimits, opts ...CallOption) (*LimitedExecutionResult, error) {
	return s.commands.RunWithLimits(ctx, command, limits, opts...)
}
