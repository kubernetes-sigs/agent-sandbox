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

package geminicli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/toolsets/geminicli/toolbox"
)

// fakeToolboxSandbox is a test double for tools.Sandbox that executes
// geminicli-toolbox invocations in-process against a local directory, mirroring
// what the real binary does inside the sandbox pod. Other commands are
// recorded for inspection.
type fakeToolboxSandbox struct {
	root  string
	calls []tools.ExecCommandOptions

	// execResult is returned for non-toolbox commands (e.g. bash).
	execResult *tools.ExecCommandResult
	// execStdout is written to opts.Stdout for non-toolbox commands.
	execStdout string
}

func (f *fakeToolboxSandbox) ExecCommand(ctx context.Context, opts tools.ExecCommandOptions) (*tools.ExecCommandResult, error) {
	f.calls = append(f.calls, opts)

	if len(opts.Command) >= 2 && opts.Command[0] == ToolboxBinary {
		var params []byte
		if opts.Stdin != nil {
			var err error
			params, err = io.ReadAll(opts.Stdin)
			if err != nil {
				return nil, err
			}
		}
		content, err := toolbox.Run(ctx, f.root, opts.Command[1], params)
		if err != nil {
			return &tools.ExecCommandResult{ExitCode: 1, Stderr: err.Error() + "\n"}, nil
		}
		return &tools.ExecCommandResult{Stdout: content}, nil
	}

	if opts.Stdout != nil && f.execStdout != "" {
		if _, err := opts.Stdout.Write([]byte(f.execStdout)); err != nil {
			return nil, err
		}
	}
	if f.execResult != nil {
		return f.execResult, nil
	}
	return &tools.ExecCommandResult{}, nil
}

func TestToolsetRegisteredTools(t *testing.T) {
	registry := tools.NewRegistry()
	New().RegisterTools(registry)

	var names []string
	for _, schema := range registry.All() {
		names = append(names, schema.Function.Name)
	}
	want := []string{
		"glob",
		"grep_search",
		"list_directory",
		"read_file",
		"read_many_files",
		"replace",
		"run_shell_command",
		"write_file",
	}
	if !slices.Equal(names, want) {
		t.Errorf("registered tools = %v, want %v", names, want)
	}

	if New().SystemPrompt() == "" {
		t.Error("SystemPrompt must not be empty")
	}
	if New().DefaultImage() == "" {
		t.Error("DefaultImage must not be empty")
	}
}

// TestToolboxToolsEndToEnd drives the host-side tools the way the registry
// does (JSON args unmarshalled into the tool struct), against the in-process
// toolbox.
func TestToolboxToolsEndToEnd(t *testing.T) {
	root := t.TempDir()
	sandbox := &fakeToolboxSandbox{root: root}
	ctx := t.Context()

	// write_file creates a file...
	writeTool := &WriteFileTool{}
	writeTool.FilePath = "hello.txt"
	writeTool.Content = "hello world"
	msg, err := writeTool.Run(ctx, sandbox)
	if err != nil {
		t.Fatalf("WriteFileTool.Run: %v", err)
	}
	if !strings.Contains(*msg.Content, "Successfully created and wrote to new file") {
		t.Errorf("write_file content = %q", *msg.Content)
	}
	if data, err := os.ReadFile(filepath.Join(root, "hello.txt")); err != nil || string(data) != "hello world" {
		t.Errorf("written file = %q, err = %v", data, err)
	}

	// ...read_file reads it back...
	readTool := &ReadFileTool{}
	readTool.FilePath = "hello.txt"
	msg, err = readTool.Run(ctx, sandbox)
	if err != nil {
		t.Fatalf("ReadFileTool.Run: %v", err)
	}
	if *msg.Content != "hello world" {
		t.Errorf("read_file content = %q", *msg.Content)
	}

	// ...and a toolbox failure is reported as model-visible content, not an
	// agent-loop error.
	missingTool := &ReadFileTool{}
	missingTool.FilePath = "missing.txt"
	msg, err = missingTool.Run(ctx, sandbox)
	if err != nil {
		t.Fatalf("ReadFileTool.Run (missing): %v", err)
	}
	if !strings.Contains(*msg.Content, "Error: tool \"read_file\" failed") {
		t.Errorf("read_file missing content = %q", *msg.Content)
	}
}

func TestRunShellCommandTool(t *testing.T) {
	sandbox := &fakeToolboxSandbox{
		root:       t.TempDir(),
		execStdout: "it worked\n",
	}

	tool := &RunShellCommandTool{Command: "echo 'it worked'", DirPath: "subdir"}
	msg, err := tool.Run(t.Context(), sandbox)
	if err != nil {
		t.Fatalf("RunShellCommandTool.Run: %v", err)
	}
	if want := "Output: it worked\n"; *msg.Content != want {
		t.Errorf("content = %q, want %q", *msg.Content, want)
	}

	if len(sandbox.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(sandbox.calls))
	}
	cmd := sandbox.calls[0].Command
	// The command must be bash -c <script> <name> <dir> <user command>, so the
	// user command is passed as a positional parameter, never interpolated.
	if len(cmd) != 6 || cmd[0] != "bash" || cmd[1] != "-c" {
		t.Fatalf("unexpected command shape: %v", cmd)
	}
	if cmd[4] != "subdir" || cmd[5] != "echo 'it worked'" {
		t.Errorf("dir/command args = %q, %q", cmd[4], cmd[5])
	}
	if strings.Contains(cmd[2], "setsid") {
		t.Errorf("foreground script should not use setsid: %q", cmd[2])
	}
}

func TestRunShellCommandToolFailure(t *testing.T) {
	sandbox := &fakeToolboxSandbox{
		root:       t.TempDir(),
		execResult: &tools.ExecCommandResult{ExitCode: 3},
	}

	tool := &RunShellCommandTool{Command: "exit 3"}
	msg, err := tool.Run(t.Context(), sandbox)
	if err != nil {
		t.Fatalf("RunShellCommandTool.Run: %v", err)
	}
	want := "Output: (empty)\nExit Code: 3"
	if *msg.Content != want {
		t.Errorf("content = %q, want %q", *msg.Content, want)
	}

	if _, err := (&RunShellCommandTool{}).Run(t.Context(), sandbox); err == nil {
		t.Error("expected error for empty command")
	}
}

func TestRunShellCommandToolBackground(t *testing.T) {
	sandbox := &fakeToolboxSandbox{root: t.TempDir()}

	tool := &RunShellCommandTool{Command: "sleep 60", IsBackground: true}
	if _, err := tool.Run(t.Context(), sandbox); err != nil {
		t.Fatalf("RunShellCommandTool.Run: %v", err)
	}
	script := sandbox.calls[0].Command[2]
	if !strings.Contains(script, "setsid") {
		t.Errorf("background script should detach with setsid: %q", script)
	}
}

// TestSchemasAreValid ensures every registered tool advertises a well-formed
// function schema.
func TestSchemasAreValid(t *testing.T) {
	registry := tools.NewRegistry()
	New().RegisterTools(registry)

	for _, schema := range registry.All() {
		if schema.Type != "function" {
			t.Errorf("tool %q: type = %q", schema.Function.Name, schema.Type)
		}
		if schema.Function.Description == "" {
			t.Errorf("tool %q: empty description", schema.Function.Name)
		}
		params, ok := schema.Function.Parameters.(map[string]any)
		if !ok {
			t.Errorf("tool %q: parameters is %T", schema.Function.Name, schema.Function.Parameters)
			continue
		}
		if params["type"] != "object" {
			t.Errorf("tool %q: parameters type = %v", schema.Function.Name, params["type"])
		}
		if _, ok := params["properties"].(map[string]any); !ok {
			t.Errorf("tool %q: missing properties", schema.Function.Name)
		}
	}
}
