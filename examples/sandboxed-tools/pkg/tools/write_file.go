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
	"bytes"
	"context"
	"fmt"
	"path/filepath"

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
)

// WriteFileTool is a tool that writes content to a file in the sandbox.
// OpenClaw uses "write" defined at https://github.com/earendil-works/pi/blob/main/packages/coding-agent/src/core/tools/write.ts
type WriteFileTool struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFileTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        "write",
			Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to write (relative or absolute)",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Content to write to the file",
					},
				},
				"required": []string{"path", "content"},
			},
		},
	}
}

func (t *WriteFileTool) Run(ctx context.Context, sandbox Sandbox) (llm.Message, error) {
	log := klog.FromContext(ctx)

	p := t.Path
	if p == "" {
		return llm.Message{}, fmt.Errorf("path is required")
	}

	dir := filepath.Dir(p)
	log.Info("creating directory in sandbox", "dir", dir)
	if _, err := sandbox.ExecCommand(ctx, ExecCommandOptions{
		// The -- separator makes execution more robust e.g. if path is "--help"
		Command: []string{"mkdir", "-p", "--", dir},
	}); err != nil {
		return llm.Message{}, fmt.Errorf("failed to create directory: %w", err)
	}

	log.Info("writing file in sandbox", "path", p)
	response, err := sandbox.ExecCommand(ctx, ExecCommandOptions{
		// We use cat instead of tee, to avoid echoing the file
		Command: []string{"sh", "-c", "cat > \"$1\"", "--", p},
		Stdin:   bytes.NewBufferString(t.Content),
	})
	if err != nil {
		return llm.Message{}, err
	}

	content := fmt.Sprintf("Wrote file %q", p)
	if response.ExitCode != 0 {
		content = fmt.Sprintf("failed to write file %q:\nstdout:\n%s\nstderr:\n%s\nexit_code: %d", p, response.Stdout, response.Stderr, response.ExitCode)
	}

	result := llm.Message{
		Content: &content,
	}
	return result, nil
}
