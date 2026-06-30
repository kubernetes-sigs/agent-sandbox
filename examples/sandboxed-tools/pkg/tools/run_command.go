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
	"fmt"

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// RunCommand is a tool that runs shell commands in the sandbox.
type RunCommand struct {
	Command string `json:"command"`
}

func (t *RunCommand) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "run_command",
			Description: "Executes a shell command inside a sandbox and returns the stdout and stderr.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The shell command to execute",
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

func (t *RunCommand) Run(ctx context.Context, sandbox Sandbox) (llm.Message, error) {
	log := klog.FromContext(ctx)

	log.Info("executing command in sandbox", "command", t.Command)
	// TODO: Add a timeout to the tool execution?
	res, err := sandbox.ExecCommand(ctx, ExecCommandOptions{
		Command: []string{"sh", "-c", t.Command},
	})
	if err != nil {
		return llm.Message{}, err
	}

	content := fmt.Sprintf("stdout:\n%s\nstderr:\n%s\nexit_code: %d", res.Stdout, res.Stderr, res.ExitCode)

	return llm.Message{
		Content: &content,
	}, nil
}
