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

package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// valueTool is a Tool implementation with value (not pointer) receivers, used
// to exercise Registry.Add's pointer-only validation.
type valueTool struct{}

func (valueTool) Schema() llm.Tool {
	return llm.Tool{Function: llm.ToolFunction{Name: "value_tool"}}
}

func (valueTool) Run(_ context.Context, _ Sandbox) (llm.Message, error) {
	return llm.Message{}, nil
}

func TestRegistryAdd_PanicsOnNilTool(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Add(nil) did not panic")
		}
		if !strings.Contains(panicString(r), "must not be nil") {
			t.Errorf("panic message = %v, want to contain %q", r, "must not be nil")
		}
	}()

	NewRegistry().Add(nil)
}

func TestRegistryAdd_PanicsOnNonPointerTool(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Add(valueTool{}) did not panic")
		}
		if !strings.Contains(panicString(r), "must be a pointer") {
			t.Errorf("panic message = %v, want to contain %q", r, "must be a pointer")
		}
	}()

	NewRegistry().Add(valueTool{})
}

func TestRegistryAll_EmptyRegistry(t *testing.T) {
	r := NewRegistry()
	if got := r.All(); len(got) != 0 {
		t.Errorf("All() = %v, want empty", got)
	}
}

func TestRegistryAll_ReturnsSchemasSortedByName(t *testing.T) {
	r := NewRegistry()
	// Registered out of alphabetical order on purpose.
	r.Add(&WriteFileTool{})
	r.Add(&RunCommand{})
	r.Add(&ListFilesTool{})
	r.Add(&ReadFileTool{})

	got := r.All()
	var gotNames []string
	for _, tool := range got {
		gotNames = append(gotNames, tool.Function.Name)
	}

	wantNames := []string{"ls", "read", "run_command", "write"}
	if diff := cmp.Diff(wantNames, gotNames); diff != "" {
		t.Errorf("tool names not sorted correctly (-want +got):\n%s", diff)
	}
}

func TestRegistryCall_ToolNotFound(t *testing.T) {
	r := NewRegistry()
	sandbox := &fakeSandbox{}

	_, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "missing", Arguments: "{}"},
	})
	if err == nil {
		t.Fatal("Call() for an unregistered tool returned nil error")
	}
	if !strings.Contains(err.Error(), `"missing" not found`) {
		t.Errorf("err = %v, want to contain %q", err, `"missing" not found`)
	}
}

func TestRegistryCall_InvalidArgumentsJSON(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	sandbox := &fakeSandbox{}

	_, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: "not json"},
	})
	if err == nil {
		t.Fatal("Call() with invalid JSON arguments returned nil error")
	}
	if !strings.Contains(err.Error(), "failed to parse arguments") {
		t.Errorf("err = %v, want to contain %q", err, "failed to parse arguments")
	}
	if len(sandbox.calls) != 0 {
		t.Error("ExecCommand was called despite invalid arguments")
	}
}

func TestRegistryCall_RunError(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	runErr := errors.New("exec failed")
	sandbox := &fakeSandbox{responses: []fakeResponse{{err: runErr}}}

	_, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"echo hi"}`},
	})
	if err == nil {
		t.Fatal("Call() returned nil error when the tool's Run failed")
	}
	if !strings.Contains(err.Error(), "failed to run tool") {
		t.Errorf("err = %v, want to contain %q", err, "failed to run tool")
	}
	if !errors.Is(err, runErr) {
		t.Errorf("err = %v, want it to wrap %v", err, runErr)
	}
}

func TestRegistryCall_Success(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "hi\n", ExitCode: 0}}},
	}

	msg, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"echo hi"}`},
	})
	if err != nil {
		t.Fatalf("Call() returned error: %v", err)
	}

	wantCmd := []string{"sh", "-c", "echo hi"}
	if diff := cmp.Diff(wantCmd, sandbox.calls[0].Command); diff != "" {
		t.Errorf("ExecCommand called with wrong command (-want +got):\n%s", diff)
	}
	if msg.Role != "tool" {
		t.Errorf("Role = %q, want %q (should default when the tool leaves it unset)", msg.Role, "tool")
	}
	if msg.ToolCallID != "call_1" {
		t.Errorf("ToolCallID = %q, want %q", msg.ToolCallID, "call_1")
	}
}

func TestRegistryCall_DoesNotMutateRegisteredTemplate(t *testing.T) {
	r := NewRegistry()
	template := &RunCommand{Command: "should-not-be-used"}
	r.Add(template)
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{ExitCode: 0}}},
	}

	_, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"echo from-call"}`},
	})
	if err != nil {
		t.Fatalf("Call() returned error: %v", err)
	}

	wantCmd := []string{"sh", "-c", "echo from-call"}
	if diff := cmp.Diff(wantCmd, sandbox.calls[0].Command); diff != "" {
		t.Errorf("ExecCommand called with wrong command (-want +got):\n%s", diff)
	}
	if template.Command != "should-not-be-used" {
		t.Errorf("registered template was mutated: Command = %q, want unchanged %q", template.Command, "should-not-be-used")
	}
}

// --- Registry.ToolTimeout ---

func TestRegistryCall_SucceedsWithTimeoutConfigured(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	r.ToolTimeout = time.Minute
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "hi\n", ExitCode: 0}}},
	}

	msg, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"echo hi"}`},
	})
	if err != nil {
		t.Fatalf("Call() returned error: %v", err)
	}
	if msg.Content == nil || !strings.Contains(*msg.Content, "hi") {
		t.Errorf("Content = %v, want it to contain %q", msg.Content, "hi")
	}
}

func TestRegistryCall_TimeoutCancelsBlockingTool(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	// A short but non-trivial real timeout: this test genuinely waits for
	// this timer to fire. Large enough to avoid flaking under CI scheduling
	// jitter, small enough to keep the test fast.
	r.ToolTimeout = 20 * time.Millisecond
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{block: true}},
	}

	_, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"sleep 99999"}`},
	})
	if err == nil {
		t.Fatal("Call() returned nil error for a tool that never returns on its own")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	wantSubstrings := []string{`"run_command"`, "timed out", r.ToolTimeout.String()}
	for _, want := range wantSubstrings {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestRegistryCall_ParentCancellationIsNotReportedAsTimeout(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	// Configured long enough that it would never fire on its own; the test
	// proves parent cancellation wins the race deterministically, not by
	// chance.
	r.ToolTimeout = time.Hour
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{block: true}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Parent is already done before Call ever runs.

	_, err := r.Call(ctx, sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"sleep 99999"}`},
	})
	if err == nil {
		t.Fatal("Call() returned nil error for a cancelled parent context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, incorrectly wraps context.DeadlineExceeded for parent cancellation", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %q, incorrectly describes parent cancellation as a timeout", err.Error())
	}
}

func TestRegistryCall_ParentDeadlineIsNotReportedAsToolTimeout(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	// Configured long enough that, if this fired on its own, it would prove
	// exactly the bug this test guards against: the registry must never
	// blame its own configured timeout for a deadline the parent already
	// owned, even though both report context.DeadlineExceeded from Err().
	r.ToolTimeout = time.Hour
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{block: true}},
	}

	// Parent deadline already in the past: context.WithTimeoutCause
	// synchronously inherits the parent's already-set cause when
	// constructing runCtx (see context.propagateCancel), so this never
	// waits on a timer.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	_, err := r.Call(ctx, sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"sleep 99999"}`},
	})
	if err == nil {
		t.Fatal("Call() returned nil error for an already-expired parent deadline")
	}
	// The parent's own deadline genuinely did expire, so this must still
	// wrap DeadlineExceeded -- just not attribute it to our ToolTimeout.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %q, incorrectly describes the parent's own expired deadline as our configured tool timeout", err.Error())
	}
	if strings.Contains(err.Error(), r.ToolTimeout.String()) {
		t.Errorf("err = %q, incorrectly claims the configured ToolTimeout (%s) fired", err.Error(), r.ToolTimeout)
	}
}

func TestRegistryCall_ZeroTimeoutDisablesWrapping(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	// r.ToolTimeout left at its zero value: timeout disabled.
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{ExitCode: 0}}},
	}

	_, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"echo hi"}`},
	})
	if err != nil {
		t.Fatalf("Call() returned error: %v", err)
	}
	if len(sandbox.ctxs) != 1 {
		t.Fatalf("ExecCommand called %d times, want 1", len(sandbox.ctxs))
	}
	if _, ok := sandbox.ctxs[0].Deadline(); ok {
		t.Error("ExecCommand received a context with a deadline despite ToolTimeout being disabled")
	}
}

func TestRegistryCall_ConfiguredTimeoutSetsDeadline(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunCommand{})
	r.ToolTimeout = time.Minute
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{ExitCode: 0}}},
	}

	_, err := r.Call(context.Background(), sandbox, llm.ToolCall{
		ID:       "call_1",
		Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"echo hi"}`},
	})
	if err != nil {
		t.Fatalf("Call() returned error: %v", err)
	}
	if len(sandbox.ctxs) != 1 {
		t.Fatalf("ExecCommand called %d times, want 1", len(sandbox.ctxs))
	}
	if _, ok := sandbox.ctxs[0].Deadline(); !ok {
		t.Error("ExecCommand did not receive a context with a deadline despite ToolTimeout being configured")
	}
}

// panicString formats a recovered panic value as a string for substring assertions.
func panicString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return ""
}
