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

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// valueTool is a Tool implementation with value (not pointer) receivers, used
// to exercise Registry.Add's pointer-only validation.
type valueTool struct{}

func (valueTool) Schema() llm.Tool {
	return llm.Tool{Function: llm.ToolFunction{Name: "value_tool"}}
}

func (valueTool) Run(ctx context.Context, sandbox Sandbox) (llm.Message, error) {
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
