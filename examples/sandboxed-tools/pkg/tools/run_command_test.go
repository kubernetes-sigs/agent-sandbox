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
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRunCommandSchema(t *testing.T) {
	tool := &RunCommand{}
	schema := tool.Schema()

	if schema.Function.Name != "run_command" {
		t.Errorf("Name = %q, want %q", schema.Function.Name, "run_command")
	}

	params, ok := schema.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("Parameters is not a map[string]any: %T", schema.Function.Parameters)
	}
	required, ok := params["required"].([]string)
	if !ok || !cmp.Equal(required, []string{"command"}) {
		t.Errorf("required = %v, want [command]", params["required"])
	}
}

func TestRunCommandRun_Success(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "hello\n", Stderr: "", ExitCode: 0}}},
	}
	tool := &RunCommand{Command: "echo hello"}

	msg, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	wantCmd := []string{"sh", "-c", "echo hello"}
	if diff := cmp.Diff(wantCmd, sandbox.calls[0].Command); diff != "" {
		t.Errorf("ExecCommand called with wrong command (-want +got):\n%s", diff)
	}

	wantContent := "stdout:\nhello\n\nstderr:\n\nexit_code: 0"
	if msg.Content == nil || *msg.Content != wantContent {
		t.Errorf("Content = %v, want %q", msg.Content, wantContent)
	}
}

func TestRunCommandRun_NonZeroExitIsNotAnError(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "", Stderr: "not found", ExitCode: 127}}},
	}
	tool := &RunCommand{Command: "nonexistent-binary"}

	msg, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error for a non-zero exit code: %v", err)
	}
	if msg.Content == nil {
		t.Fatal("Content is nil")
	}
	wantContent := "stdout:\n\nstderr:\nnot found\nexit_code: 127"
	if *msg.Content != wantContent {
		t.Errorf("Content = %q, want %q", *msg.Content, wantContent)
	}
}

func TestRunCommandRun_SandboxError(t *testing.T) {
	sandboxErr := errors.New("exec failed")
	sandbox := &fakeSandbox{responses: []fakeResponse{{err: sandboxErr}}}
	tool := &RunCommand{Command: "echo hello"}

	msg, err := tool.Run(context.Background(), sandbox)
	if !errors.Is(err, sandboxErr) {
		t.Errorf("err = %v, want %v", err, sandboxErr)
	}
	if msg.Content != nil {
		t.Errorf("Content = %v, want nil", msg.Content)
	}
}
