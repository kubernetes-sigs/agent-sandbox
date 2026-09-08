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

// The tool names, descriptions, and parameter schemas in this file are
// derived from gemini-cli (https://github.com/google-gemini/gemini-cli),
// Copyright Google LLC, licensed under the Apache License 2.0
// (packages/core/src/tools/definitions/). They are kept close to the
// originals so that model behavior tuned for gemini-cli transfers to this
// toolset.

package geminicli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/klog/v2"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/llm"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/toolsets/geminicli/toolbox"
)

// ToolboxBinary is the in-sandbox helper binary that implements the
// filesystem tools; it must be on the PATH of the sandbox image (see
// examples/sandboxed-tools/images/geminicli-toolbox).
const ToolboxBinary = "geminicli-toolbox"

// runToolbox executes one toolbox tool inside the sandbox: it sends the tool
// parameters as JSON on stdin to `geminicli-toolbox <name>` and returns the tool
// result. Tool failures (non-zero exit) are reported to the model as content,
// so the model can correct its arguments and retry.
func runToolbox(ctx context.Context, sandbox tools.Sandbox, name string, params any) (llm.Message, error) {
	log := klog.FromContext(ctx)
	log.Info("running toolbox tool in sandbox", "tool.name", name)

	payload, err := json.Marshal(params)
	if err != nil {
		return llm.Message{}, fmt.Errorf("failed to encode tool parameters: %w", err)
	}

	res, err := sandbox.ExecCommand(ctx, tools.ExecCommandOptions{
		Command: []string{ToolboxBinary, name},
		Stdin:   bytes.NewReader(payload),
	})
	if err != nil {
		return llm.Message{}, err
	}

	content := res.Stdout
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(strings.Join([]string{res.Stderr, res.Stdout}, "\n"))
		content = fmt.Sprintf("Error: tool %q failed (exit code %d): %s", name, res.ExitCode, detail)
	}
	return llm.Message{Content: &content}, nil
}

// objectSchema is a small helper to build JSON schemas for tool parameters.
func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// ListDirectoryTool lists the entries of a directory in the sandbox.
type ListDirectoryTool struct {
	toolbox.ListDirectoryParams
}

func (t *ListDirectoryTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        toolbox.ListDirectoryToolName,
			Description: "Lists the names of files and subdirectories directly within a specified directory path. Can optionally ignore entries matching provided glob patterns.",
			Parameters: objectSchema(map[string]any{
				"dir_path": map[string]any{
					"type":        "string",
					"description": "The path to the directory to list",
				},
				"ignore": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "List of glob patterns to ignore",
				},
			}, "dir_path"),
		},
	}
}

func (t *ListDirectoryTool) Run(ctx context.Context, sandbox tools.Sandbox) (llm.Message, error) {
	return runToolbox(ctx, sandbox, toolbox.ListDirectoryToolName, t)
}

// ReadFileTool reads a file from the sandbox.
//
// TODO: gemini-cli's read_file also handles images (PNG, JPG, GIF, WEBP,
// SVG, BMP), audio, and PDF files by returning them as inline base64 parts.
// We only support text files (binary files are rejected); multimodal support
// would also need pkg/llm to grow multi-part message content.
type ReadFileTool struct {
	toolbox.ReadFileParams
}

func (t *ReadFileTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        toolbox.ReadFileToolName,
			Description: "Reads and returns the content of a specified text file. If the file is large, the content will be truncated. The tool's response will clearly indicate if truncation has occurred and will provide details on how to read more of the file using the 'start_line' and 'end_line' parameters.",
			Parameters: objectSchema(map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "The path to the file to read.",
				},
				"start_line": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional: The 1-based line number to start reading from.",
				},
				"end_line": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional: The 1-based line number to end reading at (inclusive).",
				},
			}, "file_path"),
		},
	}
}

func (t *ReadFileTool) Run(ctx context.Context, sandbox tools.Sandbox) (llm.Message, error) {
	return runToolbox(ctx, sandbox, toolbox.ReadFileToolName, t)
}

// WriteFileTool writes a file in the sandbox.
type WriteFileTool struct {
	toolbox.WriteFileParams
}

func (t *WriteFileTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        toolbox.WriteFileToolName,
			Description: "Writes content to a specified file in the sandbox filesystem, creating parent directories if needed and overwriting any existing content.",
			Parameters: objectSchema(map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "The path to the file to write to.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The content to write to the file. Do not use omission placeholders like '(rest of methods ...)', '...', or 'unchanged code'; provide complete literal content.",
				},
			}, "file_path", "content"),
		},
	}
}

func (t *WriteFileTool) Run(ctx context.Context, sandbox tools.Sandbox) (llm.Message, error) {
	return runToolbox(ctx, sandbox, toolbox.WriteFileToolName, t)
}

// GlobTool finds files matching glob patterns in the sandbox.
type GlobTool struct {
	toolbox.GlobParams
}

func (t *GlobTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        toolbox.GlobToolName,
			Description: "Efficiently finds files matching specific glob patterns (e.g., `src/**/*.ts`, `**/*.md`), returning paths sorted by modification time (newest first). Ideal for quickly locating files based on their name or path structure, especially in large codebases.",
			Parameters: objectSchema(map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "The glob pattern to match against (e.g., '**/*.py', 'docs/*.md').",
				},
				"dir_path": map[string]any{
					"type":        "string",
					"description": "Optional: The path to the directory to search within. If omitted, searches the workspace root directory.",
				},
				"case_sensitive": map[string]any{
					"type":        "boolean",
					"description": "Optional: Whether the search should be case-sensitive. Defaults to false.",
				},
			}, "pattern"),
		},
	}
}

func (t *GlobTool) Run(ctx context.Context, sandbox tools.Sandbox) (llm.Message, error) {
	return runToolbox(ctx, sandbox, toolbox.GlobToolName, t)
}

// GrepTool searches file contents in the sandbox.
type GrepTool struct {
	toolbox.GrepParams
}

func (t *GrepTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        toolbox.GrepToolName,
			Description: "Searches for a regular expression pattern within file contents.",
			Parameters: objectSchema(map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": `The pattern to search for. By default, treated as a regular expression. Use '\b' for precise symbol matching (e.g., '\bMatchMe\b').`,
				},
				"dir_path": map[string]any{
					"type":        "string",
					"description": "Directory or file to search. Directories are searched recursively. Relative paths are resolved against the workspace root. Defaults to the workspace root if omitted.",
				},
				"include_pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern to filter files (e.g., '*.ts', 'src/**'). Recommended for large repositories to reduce noise. Defaults to all files if omitted.",
				},
				"exclude_pattern": map[string]any{
					"type":        "string",
					"description": "Optional: A regular expression pattern to exclude from the search results. If a line matches both the pattern and the exclude_pattern, it will be omitted.",
				},
				"names_only": map[string]any{
					"type":        "boolean",
					"description": "Optional: If true, only the file paths of the matches will be returned, without the line content or line numbers. This is useful for gathering a list of files.",
				},
				"case_sensitive": map[string]any{
					"type":        "boolean",
					"description": "If true, search is case-sensitive. Defaults to false (ignore case) if omitted.",
				},
				"fixed_strings": map[string]any{
					"type":        "boolean",
					"description": "If true, treats the `pattern` as a literal string instead of a regular expression. Defaults to false (regex) if omitted.",
				},
				"context": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Show this many lines of context around each match (equivalent to grep -C). Defaults to 0 if omitted.",
				},
				"after": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Show this many lines after each match (equivalent to grep -A). Defaults to 0 if omitted.",
				},
				"before": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Show this many lines before each match (equivalent to grep -B). Defaults to 0 if omitted.",
				},
				"max_matches_per_file": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional: Maximum number of matches to return per file. Use this to prevent being overwhelmed by repetitive matches in large files.",
				},
				"total_max_matches": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"description": "Optional: Maximum number of total matches to return. Use this to limit the overall size of the response. Defaults to 100 if omitted.",
				},
			}, "pattern"),
		},
	}
}

func (t *GrepTool) Run(ctx context.Context, sandbox tools.Sandbox) (llm.Message, error) {
	return runToolbox(ctx, sandbox, toolbox.GrepToolName, t)
}

// ReplaceTool performs an exact-literal-string edit of a file in the sandbox.
type ReplaceTool struct {
	toolbox.ReplaceParams
}

func (t *ReplaceTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name: toolbox.ReplaceToolName,
			Description: "Replaces text within a file. By default, the tool expects to find and replace exactly ONE occurrence of `old_string`. If you want to replace multiple occurrences of the exact same string, set `allow_multiple` to true. This tool requires providing significant context around the change to ensure precise targeting. Always use the read_file tool to examine the file's current content before attempting a text replacement.\n" +
				"Expectation for required parameters:\n" +
				"1. `old_string` MUST be the exact literal text to replace (including all whitespace, indentation, newlines, and surrounding code etc.).\n" +
				"2. `new_string` MUST be the exact literal text to replace `old_string` with (also including all whitespace, indentation, newlines, and surrounding code etc.). Ensure the resulting code is correct and idiomatic and that `old_string` and `new_string` are different.\n" +
				"3. `instruction` is the detailed instruction of what needs to be changed. Make it specific and detailed so developers or large language models can understand what needs to be changed and perform the changes on their own if necessary.\n" +
				"4. NEVER escape `old_string` or `new_string`, that would break the exact literal text requirement.\n" +
				"**Important:** If ANY of the above are not satisfied, the tool will fail. CRITICAL for `old_string`: Must uniquely identify the instance(s) to change. Include at least 3 lines of context BEFORE and AFTER the target text, matching whitespace and indentation precisely. If this string matches multiple locations and `allow_multiple` is not true, the tool will fail.\n" +
				"5. Prefer to break down complex and long changes into multiple smaller atomic calls to this tool. Always check the content of the file after changes or not finding a string to match.\n" +
				"**Multiple replacements:** Set `allow_multiple` to true if you want to replace ALL occurrences that match `old_string` exactly.",
			Parameters: objectSchema(map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "The path to the file to modify.",
				},
				"instruction": map[string]any{
					"type": "string",
					"description": "A clear, semantic instruction for the code change, acting as a high-quality prompt for an expert LLM assistant. It must be self-contained and explain the goal of the change.\n" +
						"A good instruction should concisely answer:\n" +
						"1. WHY is the change needed?\n2. WHERE should the change happen?\n3. WHAT is the high-level change?\n4. WHAT is the desired outcome?",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "The exact literal text to replace, preferably unescaped. For single replacements (default), include at least 3 lines of context BEFORE and AFTER the target text, matching whitespace and indentation precisely. If this string is not the exact literal text (i.e. you escaped it) or does not match exactly, the tool will fail.",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "The exact literal text to replace `old_string` with, preferably unescaped. Provide the EXACT text. Ensure the resulting code is correct and idiomatic. Do not use omission placeholders like '(rest of methods ...)', '...', or 'unchanged code'; provide exact literal code.",
				},
				"allow_multiple": map[string]any{
					"type":        "boolean",
					"description": "If true, the tool will replace all occurrences of `old_string`. If false (default), it will only succeed if exactly one occurrence is found.",
				},
			}, "file_path", "instruction", "old_string", "new_string"),
		},
	}
}

func (t *ReplaceTool) Run(ctx context.Context, sandbox tools.Sandbox) (llm.Message, error) {
	return runToolbox(ctx, sandbox, toolbox.ReplaceToolName, t)
}

// ReadManyFilesTool reads several files from the sandbox at once.
type ReadManyFilesTool struct {
	toolbox.ReadManyFilesParams
}

func (t *ReadManyFilesTool) Schema() llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name: toolbox.ReadManyFilesToolName,
			Description: "Reads content from multiple text files specified by glob patterns, concatenating their content into a single string with a '--- {filePath} ---' separator between file contents and '--- End of content ---' after the last file.\n" +
				"This tool is useful when you need to understand or analyze a collection of files, such as getting an overview of a codebase or parts of it, reviewing documentation files, or gathering context from multiple configuration files. Binary files are skipped. Default excludes apply to common dependency and build directories unless 'useDefaultExcludes' is false.",
			Parameters: objectSchema(map[string]any{
				"include": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string", "minLength": 1},
					"minItems":    1,
					"description": `An array of glob patterns or paths, relative to the workspace root. Examples: ["src/**/*.ts"], ["README.md", "docs/"]`,
				},
				"exclude": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string", "minLength": 1},
					"description": `Optional. Glob patterns for files/directories to exclude. Added to default excludes if useDefaultExcludes is true. Example: "**/*.log", "temp/"`,
				},
				"recursive": map[string]any{
					"type":        "boolean",
					"description": "Optional. Whether to search recursively (primarily controlled by `**` in glob patterns). Defaults to true.",
				},
				"useDefaultExcludes": map[string]any{
					"type":        "boolean",
					"description": "Optional. Whether to apply a list of default exclusion patterns (e.g., node_modules, .git, binary files). Defaults to true.",
				},
			}, "include"),
		},
	}
}

func (t *ReadManyFilesTool) Run(ctx context.Context, sandbox tools.Sandbox) (llm.Message, error) {
	return runToolbox(ctx, sandbox, toolbox.ReadManyFilesToolName, t)
}
