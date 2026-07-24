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
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestReadFileSchema(t *testing.T) {
	tool := &ReadFileTool{}
	schema := tool.Schema()

	if schema.Function.Name != "read" {
		t.Errorf("Name = %q, want %q", schema.Function.Name, "read")
	}

	params, ok := schema.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("Parameters is not a map[string]any: %T", schema.Function.Parameters)
	}
	required, ok := params["required"].([]string)
	if !ok || !cmp.Equal(required, []string{"path"}) {
		t.Errorf("required = %v, want [path]", params["required"])
	}
}

func TestReadFileRun_EmptyPathIsRejected(t *testing.T) {
	sandbox := &fakeSandbox{}
	tool := &ReadFileTool{Path: ""}

	_, err := tool.Run(context.Background(), sandbox)
	if err == nil {
		t.Fatal("Run() with empty path returned nil error")
	}
	if len(sandbox.calls) != 0 {
		t.Error("ExecCommand was called for an empty path")
	}
}

func TestReadFileRun_Success(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "file contents", ExitCode: 0}}},
	}
	tool := &ReadFileTool{Path: "/tmp/foo.txt"}

	msg, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	wantCmd := []string{"cat", "--", "/tmp/foo.txt"}
	if diff := cmp.Diff(wantCmd, sandbox.calls[0].Command); diff != "" {
		t.Errorf("ExecCommand called with wrong command (-want +got):\n%s", diff)
	}

	if msg.Content == nil || *msg.Content != "file contents" {
		t.Errorf("Content = %v, want %q", msg.Content, "file contents")
	}
}

func TestReadFileRun_NonZeroExitReturnsFailureMessage(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "", Stderr: "No such file or directory", ExitCode: 1}}},
	}
	tool := &ReadFileTool{Path: "/tmp/missing.txt"}

	msg, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error for a non-zero exit code: %v", err)
	}

	wantContent := fmt.Sprintf("Failed to read %q:\nstdout:\n%s\nstderr:\n%s\nexit_code: %d",
		"/tmp/missing.txt", "", "No such file or directory", 1)
	if msg.Content == nil || *msg.Content != wantContent {
		t.Errorf("Content = %v, want %q", msg.Content, wantContent)
	}
}

func TestReadFileRun_SandboxError(t *testing.T) {
	sandboxErr := errors.New("exec failed")
	sandbox := &fakeSandbox{responses: []fakeResponse{{err: sandboxErr}}}
	tool := &ReadFileTool{Path: "/tmp/foo.txt"}

	msg, err := tool.Run(context.Background(), sandbox)
	if !errors.Is(err, sandboxErr) {
		t.Errorf("err = %v, want %v", err, sandboxErr)
	}
	if msg.Content != nil {
		t.Errorf("Content = %v, want nil", msg.Content)
	}
}
