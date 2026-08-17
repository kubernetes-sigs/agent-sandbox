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

func TestListFilesSchema(t *testing.T) {
	tool := &ListFilesTool{}
	schema := tool.Schema()

	if schema.Function.Name != "ls" {
		t.Errorf("Name = %q, want %q", schema.Function.Name, "ls")
	}

	params, ok := schema.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("Parameters is not a map[string]any: %T", schema.Function.Parameters)
	}
	// path is optional (defaults to the current directory), so it must not
	// be listed as required.
	if _, hasRequired := params["required"]; hasRequired {
		t.Errorf("required = %v, want no required fields", params["required"])
	}
}

func TestListFilesRun_DefaultsToCurrentDirectory(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "a.txt\nb.txt\n", ExitCode: 0}}},
	}
	tool := &ListFilesTool{Path: ""}

	msg, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	wantCmd := []string{"ls", "-p", "--", "."}
	if diff := cmp.Diff(wantCmd, sandbox.calls[0].Command); diff != "" {
		t.Errorf("ExecCommand called with wrong command (-want +got):\n%s", diff)
	}
	if msg.Content == nil || *msg.Content != "a.txt\nb.txt\n" {
		t.Errorf("Content = %v, want %q", msg.Content, "a.txt\nb.txt\n")
	}
}

func TestListFilesRun_UsesGivenPath(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "sub/\n", ExitCode: 0}}},
	}
	tool := &ListFilesTool{Path: "/tmp/dir"}

	_, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	wantCmd := []string{"ls", "-p", "--", "/tmp/dir"}
	if diff := cmp.Diff(wantCmd, sandbox.calls[0].Command); diff != "" {
		t.Errorf("ExecCommand called with wrong command (-want +got):\n%s", diff)
	}
}

func TestListFilesRun_NonZeroExitReturnsFailureMessage(t *testing.T) {
	sandbox := &fakeSandbox{
		responses: []fakeResponse{{result: &ExecCommandResult{Stdout: "", Stderr: "No such file or directory", ExitCode: 2}}},
	}
	tool := &ListFilesTool{Path: "/tmp/missing"}

	msg, err := tool.Run(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Run() returned error for a non-zero exit code: %v", err)
	}

	wantContent := fmt.Sprintf("Failed to list %q:\nstdout:\n%s\nstderr:\n%s\nexit_code: %d",
		"/tmp/missing", "", "No such file or directory", 2)
	if msg.Content == nil || *msg.Content != wantContent {
		t.Errorf("Content = %v, want %q", msg.Content, wantContent)
	}
}

func TestListFilesRun_SandboxError(t *testing.T) {
	sandboxErr := errors.New("exec failed")
	sandbox := &fakeSandbox{responses: []fakeResponse{{err: sandboxErr}}}
	tool := &ListFilesTool{Path: "/tmp/dir"}

	msg, err := tool.Run(context.Background(), sandbox)
	if !errors.Is(err, sandboxErr) {
		t.Errorf("err = %v, want %v", err, sandboxErr)
	}
	if msg.Content != nil {
		t.Errorf("Content = %v, want nil", msg.Content)
	}
}
