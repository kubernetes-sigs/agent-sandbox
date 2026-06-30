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

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// ReadFileTool is a tool that reads file contents from the sandbox.
type ReadFileTool struct {
	Path string `json:"path"`
}

func (t *ReadFileTool) Schema() llm.Tool {
	// Area of future investigation: do the models favor certain names (e.g. antigravity uses view_file)
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "read",
			Description: "Read the contents of a file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to read (relative or absolute)",
					},
				},
				"required": []string{"path"},
			},
		},
	}
}

func (t *ReadFileTool) Run(ctx context.Context, sandbox Sandbox) (llm.Message, error) {
	log := klog.FromContext(ctx)

	if t.Path == "" {
		return llm.Message{}, errors.New("path is required")
	}

	log.Info("reading file in sandbox", "path", t.Path)
	res, err := sandbox.ExecCommand(ctx, ExecCommandOptions{
		// The -- separator makes execution more robust e.g. if path is "--help"
		Command: []string{"cat", "--", t.Path},
	})
	if err != nil {
		return llm.Message{}, err
	}

	content := res.Stdout

	if res.ExitCode != 0 {
		content = fmt.Sprintf("Failed to read %q:\nstdout:\n%s\nstderr:\n%s\nexit_code: %d", t.Path, res.Stdout, res.Stderr, res.ExitCode)
	}

	result := llm.Message{
		Content: &content,
	}

	return result, nil
}
