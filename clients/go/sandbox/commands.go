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
func (c *Commands) Run(ctx context.Context, command string, opts ...CallOption) (*ExecutionResult, error) {
	defer c.trackOp()()
	ctx, callCancel, maxAttempts := applyCallOpts(ctx, opts)
	defer callCancel()
	if maxAttempts == 0 {
		maxAttempts = 1 // safe default: no retries for non-idempotent commands
	}
	ctx = withLifecycleSpan(ctx, c.lifecycleCtx())
	ctx, span := startSpan(ctx, c.tracer, c.svcName, "run", AttrCommandExecutable.String(commandExecutable(command)))
	defer func() { span.End() }()

	if c.runtime == RuntimeSandboxd {
		return c.runSandboxd(ctx, span, command)
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
func (c *Commands) runSandboxd(ctx context.Context, span trace.Span, command string) (*ExecutionResult, error) {
	conn, err := c.connector.GRPCConn()
	if err != nil {
		recordError(span, err)
		return nil, fmt.Errorf("%s: run failed: %w", c.errPrefix(), err)
	}
	client := processv1.NewProcessServiceClient(conn)

	resp, err := client.Execute(ctx, &processv1.ExecuteRequest{
		Config: &processv1.ProcessConfig{
			Command: []string{"/bin/sh", "-c", command},
		},
	})
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
