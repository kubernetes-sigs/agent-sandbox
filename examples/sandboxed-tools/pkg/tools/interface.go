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
	"io"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// Tool is the interface for all LLM-callable tools.
type Tool interface {
	Schema() llm.Tool
	Run(ctx context.Context, sandbox Sandbox) (llm.Message, error)
}

// Sandbox represents the interface for running commands in a sandbox.
type Sandbox interface {
	// ExecCommand executes a command inside the sandbox container with specified options.
	// If Stdout or Stderr are nil in tools.ExecCommandOptions, they are captured internally and returned in the tools.ExecCommandResult.
	// If the command returns a non-zero exit code, that is _not_ treated as an error; the exit code is returned in the result.
	ExecCommand(ctx context.Context, opts ExecCommandOptions) (*ExecCommandResult, error)
}

// ExecCommandResult holds the result of running a command in the sandbox.
type ExecCommandResult struct {
	// Stdout contains the captured standard output.
	// This is only populated if ExecCommandOptions.Stdout was nil.
	Stdout string

	// Stderr contains the captured standard error.
	// This is only populated if ExecCommandOptions.Stderr was nil.
	Stderr string

	// ExitCode is the exit status returned by the command.
	ExitCode int
}

// ExecCommandOptions holds the options for running a command in the sandbox.
type ExecCommandOptions struct {

	// Command is the process name and arguments to run (e.g. []string{"sh", "-c", "uname -a"}).
	Command []string

	// Stdin is an optional reader to pipe into the command's standard input.
	Stdin io.Reader

	// Stdout is an optional writer where standard output will be written.
	// If nil, Stdout will be captured internally and returned as a string in the ExecCommandResult.
	Stdout io.Writer

	// Stderr is an optional writer where standard error will be written.
	// If nil, Stderr will be captured internally and returned as a string in the ExecCommandResult.
	Stderr io.Writer
}
