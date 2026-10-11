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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
)

const maxExecutionResponseSize = 16 << 20 // 16 MB

// Commands provides command execution on a sandbox.
type Commands struct {
	connector    *connector
	runtime      Runtime
	tracer       trace.Tracer
	svcName      string
	log          logr.Logger
	errPrefix    func() string
	trackOp      func() func()
	lifecycleCtx func() context.Context
}

// Run executes a command in the sandbox and returns the result.
// The combined JSON response (stdout + stderr + metadata) is limited to 16 MB;
// commands producing more output will fail with ErrResponseTooLarge.
//
// Because command execution is not idempotent, Run defaults to a single
// attempt (no retries). For idempotent commands that should retry on
// transient server errors (502, 503, etc.), use WithMaxAttempts:
//
//	result, err := client.Run(ctx, "cat /etc/hostname", sandbox.WithMaxAttempts(6))
//
// WithMaxAttempts applies only to the legacy runtime. With RuntimeSandboxd,
// Run issues a single gRPC Execute regardless of the configured attempts.
//
// WithEnv and WithWorkingDir apply only to RuntimeSandboxd; the legacy runtime
// returns ErrUnsupportedByRuntime rather than silently ignoring them.
func (c *Commands) Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error) {
	defer c.trackOp()()
	co := resolveCallOpts(opts)
	// No retries by default: commands are not idempotent.
	ctx, callCancel, maxAttempts := applyCallOptsNoRetry(ctx, opts)
	defer callCancel()
	ctx = withLifecycleSpan(ctx, c.lifecycleCtx())
	ctx, span := startSpan(ctx, c.tracer, c.svcName, "run", AttrCommandExecutable.String(commandExecutable(command)))
	defer func() { span.End() }()

	if c.runtime == RuntimeSandboxd {
		return c.runSandboxd(ctx, span, command, co)
	}
	if len(co.env) > 0 || co.cwd != "" {
		err := fmt.Errorf("%s: run: %w: the legacy python-runtime ignores WithEnv and WithWorkingDir", c.errPrefix(), ErrUnsupportedByRuntime)
		recordError(span, err)
		return nil, err
	}

	payload, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: failed to marshal command: %w", c.errPrefix(), err)
	}

	resp, err := c.connector.SendRequest(ctx, http.MethodPost, "execute", bytes.NewReader(payload), "application/json", maxAttempts)
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: run failed: %w", c.errPrefix(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		retErr := fmt.Errorf("%s: run: %w", c.errPrefix(), &HTTPError{StatusCode: resp.StatusCode, Body: string(body), Operation: "run"})
		recordError(span, retErr)
		return nil, retErr
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes)) }()

	result := ExecutionResult{ExitCode: -1}
	lr := io.LimitedReader{R: resp.Body, N: maxExecutionResponseSize}
	if err := json.NewDecoder(&lr).Decode(&result); err != nil {
		if lr.N <= 0 {
			err = fmt.Errorf("%s: %w: command output too large", c.errPrefix(), ErrResponseTooLarge)
		} else {
			err = fmt.Errorf("%s: failed to decode run result: %w", c.errPrefix(), err)
		}
		recordError(span, err)
		return nil, err
	}
	span.SetAttributes(AttrExitCode.Int(result.ExitCode))
	c.log.V(1).Info("run completed", "exitCode", result.ExitCode)
	return &result, nil
}

// runSandboxd executes the command through sandboxd's gRPC ProcessService.
// The shell-string API is preserved by wrapping the command in "/bin/sh -c",
// matching what the legacy python-runtime did server-side.
func (c *Commands) runSandboxd(ctx context.Context, span trace.Span, command string, co callOptions) (*ExecutionResult, error) {
	config, err := commandConfig(command, co)
	if err != nil {
		err = fmt.Errorf("%s: run: %w", c.errPrefix(), err)
		recordError(span, err)
		return nil, err
	}

	conn, err := c.connector.GRPCConn()
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: run failed: %w", c.errPrefix(), err)
	}
	client := processv1.NewProcessServiceClient(conn)

	resp, err := client.Execute(ctx, &processv1.ExecuteRequest{Config: config})
	if err != nil {
		if st, ok := status.FromError(err); ok {
			err = fmt.Errorf("%s: run: process service returned %s: %s: %w", c.errPrefix(), st.Code(), st.Message(), err)
		} else {
			err = fmt.Errorf("%s: run failed: %w", c.errPrefix(), err)
		}
		recordError(span, err)
		return nil, err
	}

	result := &ExecutionResult{
		Stdout:   string(resp.GetStdout()),
		Stderr:   string(resp.GetStderr()),
		ExitCode: int(resp.GetExitCode()),
	}
	span.SetAttributes(AttrExitCode.Int(result.ExitCode))
	c.log.V(1).Info("run completed", "exitCode", result.ExitCode)
	return result, nil
}

// Start starts a sandboxd command and returns after receiving its process ID.
// It preserves Run's shell-string, WithEnv and WithWorkingDir semantics.
// The caller must Recv through the exit event to io.EOF, or Close the handle.
// The context and an explicit WithTimeout cover the entire stream; there is
// no default RequestTimeout. Start never retries and rejects WithMaxAttempts > 1.
// The legacy runtime and REST-only connections return ErrUnsupportedByRuntime.
func (c *Commands) Start(ctx context.Context, command string, opts ...CallOption) (handle *ProcessHandle, err error) {
	done := c.trackOp()
	co := resolveCallOpts(opts)
	var cancel context.CancelFunc
	if co.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, co.timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	ctx, span := startSpan(withLifecycleSpan(ctx, c.lifecycleCtx()), c.tracer, c.svcName, "start", AttrCommandExecutable.String(commandExecutable(command)))
	defer func() {
		if handle == nil {
			recordError(span, err)
			cancel()
			done()
			span.End()
		}
	}()
	prefix := c.errPrefix()
	if c.runtime != RuntimeSandboxd {
		return nil, fmt.Errorf("%s: start: %w", prefix, ErrUnsupportedByRuntime)
	}
	if co.maxAttempts > 1 {
		return nil, fmt.Errorf("%s: start: WithMaxAttempts greater than 1 is unsupported: commands cannot be replayed", prefix)
	}
	config, err := commandConfig(command, co)
	if err != nil {
		return nil, fmt.Errorf("%s: start: %w", prefix, err)
	}
	conn, err := c.connector.GRPCConn()
	if err != nil {
		return nil, fmt.Errorf("%s: start: %w", prefix, err)
	}
	client := processv1.NewProcessServiceClient(conn)
	// Commit the sent request so service-config retries cannot start it twice.
	stream, err := client.Start(ctx, &processv1.StartRequest{Config: config}, grpc.MaxRetryRPCBufferSize(0))
	if err != nil {
		return nil, fmt.Errorf("%s: start: %w", prefix, err)
	}
	init, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("%s: start: %w: missing Init: %w", prefix, ErrProcessProtocol, io.ErrUnexpectedEOF)
		}
		return nil, fmt.Errorf("%s: start: %w", prefix, err)
	}
	if init.GetInit() == nil || init.GetInit().GetProcessId() == 0 {
		return nil, fmt.Errorf("%s: start: %w: expected Init with a nonzero process ID", prefix, ErrProcessProtocol)
	}
	handle = &ProcessHandle{
		stream: stream, client: client, pid: init.GetInit().GetProcessId(),
		ctx: ctx, prefix: prefix, span: span,
		cleanup: func() { cancel(); done(); span.End() },
	}
	// Cancellation and ClientConn.Close finish the stream even without Recv.
	// Observe lifecycle only: never drain or buffer the caller's output.
	context.AfterFunc(stream.Context(), handle.observeCompletion)
	return handle, nil
}

func commandConfig(command string, co callOptions) (*processv1.ProcessConfig, error) {
	for k := range co.env {
		// sandboxd joins these as KEY=value, so such a key would corrupt it.
		if k == "" || strings.Contains(k, "=") {
			return nil, fmt.Errorf("invalid env key %q: must be non-empty and contain no '='", k)
		}
	}
	config := &processv1.ProcessConfig{Command: []string{"/bin/sh", "-c", command}, EnvVars: co.env}
	if co.cwd != "" {
		config.Cwd = &co.cwd
	}
	return config, nil
}

func commandExecutable(command string) string {
	fields := strings.FieldsSeq(command)
	for field := range fields {
		// Skip leading inline environment variables (e.g., KEY=VALUE)
		if strings.Contains(field, "=") {
			continue
		}
		// Extract base executable name (strip directory paths)
		parts := strings.Split(field, "/")
		return parts[len(parts)-1]
	}
	return ""
}
