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
	"io"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestWriteFileSchema(t *testing.T) {
	tool := &WriteFileTool{}
	schema := tool.Schema()

	if schema.Function.Name != "write" {
		t.Errorf("Name = %q, want %q", schema.Function.Name, "write")
	}

	params, ok := schema.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("Parameters is not a map[string]any: %T", schema.Function.Parameters)
	}
	required, ok := params["required"].([]string)
	if !ok || !cmp.Equal(required, []string{"path", "content"}) {
		t.Errorf("required = %v, want [path content]", params["required"])
	}
}

func TestWriteFileRun_EmptyPathIsRejected(t *testing.T) {
	sandbox := &fakeSandbox{}
	tool := &WriteFileTool{Path: "", Content: "hello"}

	_, err := tool.Run(context.Background(), sandbox)
	if err == nil {
		t.Fatal("Run() with empty path returned nil error")
	}
	if len(sandbox.calls) != 0 {
		t.Error("ExecCommand was called for an empty path")
	}
}

func TestWriteFileRun_Success(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{
			{result: &ExecCommandResult{ExitCode: 0}}, // mkdir -p
			{result: &ExecCommandResult{ExitCode: 0}}, // write
		},
	}
	tool := &WriteFileTool{Path: "/tmp/sub/foo.txt", Content: "hello world"}

	msg, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}
	if len(sandbox.calls) != 2 {
		t.Fatalf("ExecCommand called %d times, want 2", len(sandbox.calls))
	}

	wantMkdirCmd := []string{"mkdir", "-p", "--", "/tmp/sub"}
	if diff := cmp.Diff(wantMkdirCmd, sandbox.calls[0].Command); diff != "" {
		t.Errorf("mkdir command mismatch (-want +got):\n%s", diff)
	}

	wantWriteCmd := []string{"sh", "-c", `cat > "$1"`, "--", "/tmp/sub/foo.txt"}
	if diff := cmp.Diff(wantWriteCmd, sandbox.calls[1].Command); diff != "" {
		t.Errorf("write command mismatch (-want +got):\n%s", diff)
	}
	gotStdin, err := io.ReadAll(sandbox.calls[1].Stdin)
	if err != nil {
		t.Fatalf("failed to read stdin passed to write command: %v", err)
	}
	if string(gotStdin) != "hello world" {
		t.Errorf("stdin = %q, want %q", gotStdin, "hello world")
	}

	wantContent := `Wrote file "/tmp/sub/foo.txt"`
	if msg.Content == nil || *msg.Content != wantContent {
		t.Errorf("Content = %v, want %q", msg.Content, wantContent)
	}
}

func TestWriteFileRun_MkdirFailureStopsBeforeWrite(t *testing.T) {
	mkdirErr := errors.New("permission denied")
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{err: mkdirErr}},
	}
	tool := &WriteFileTool{Path: "/root/foo.txt", Content: "hello"}

	msg, err := tool.Run(context.Background(), sandbox)
	if !errors.Is(err, mkdirErr) {
		t.Errorf("err = %v, want wrapped %v", err, mkdirErr)
	}
	if msg.Content != nil {
		t.Errorf("Content = %v, want nil", msg.Content)
	}
	if len(sandbox.calls) != 1 {
		t.Errorf("ExecCommand called %d times, want 1 (write should not be attempted)", len(sandbox.calls))
	}
}

func TestWriteFileRun_WriteExecError(t *testing.T) {
	writeErr := errors.New("exec failed")
	sandbox := &fakeSandbox{
		responses: []fakeResponse{
			{result: &ExecCommandResult{ExitCode: 0}}, // mkdir -p succeeds
			{err: writeErr},
		},
	}
	tool := &WriteFileTool{Path: "/tmp/foo.txt", Content: "hello"}

	msg, err := tool.Run(context.Background(), sandbox)
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want %v", err, writeErr)
	}
	if msg.Content != nil {
		t.Errorf("Content = %v, want nil", msg.Content)
	}
}

func TestWriteFileRun_NonZeroExitOnWrite(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{
			{result: &ExecCommandResult{ExitCode: 0}},
			{result: &ExecCommandResult{Stdout: "", Stderr: "disk full", ExitCode: 1}},
		},
	}
	tool := &WriteFileTool{Path: "/tmp/foo.txt", Content: "hello"}

	msg, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error for a non-zero exit code: %v", err)
	}

	wantContent := fmt.Sprintf("failed to write file %q:\nstdout:\n%s\nstderr:\n%s\nexit_code: %d",
		"/tmp/foo.txt", "", "disk full", 1)
	if msg.Content == nil || *msg.Content != wantContent {
		t.Errorf("Content = %v, want %q", msg.Content, wantContent)
	}
}
