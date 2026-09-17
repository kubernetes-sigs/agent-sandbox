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

// Package toolbox implements the filesystem tools of the geminicli toolset in
// Go. It is compiled into the geminicli-toolbox binary, which runs *inside* the
// sandbox: the host-side tools send the LLM's tool arguments as JSON on stdin
// and receive the model-facing result on stdout.
//
// The tool names, parameters, and result formats follow gemini-cli
// (https://github.com/google-gemini/gemini-cli, Apache-2.0, Copyright Google
// LLC), reimplemented in Go so the sandbox image does not need Node.js.
package toolbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Tool names understood by Run. These match gemini-cli's tool names so that
// prompts and model behavior transfer directly.
const (
	ListDirectoryToolName = "list_directory"
	ReadFileToolName      = "read_file"
	WriteFileToolName     = "write_file"
	GlobToolName          = "glob"
	GrepToolName          = "grep_search"
	ReplaceToolName       = "replace"
	ReadManyFilesToolName = "read_many_files"
)

// Run executes the named tool with the given JSON parameters. Relative paths
// in the parameters are resolved against root. The returned string is the
// model-facing tool result; a returned error is also model-facing (it
// describes why the tool call failed, e.g. "file not found").
func Run(ctx context.Context, root string, toolName string, paramsJSON []byte) (string, error) {
	switch toolName {
	case ListDirectoryToolName:
		var params ListDirectoryParams
		if err := unmarshalParams(paramsJSON, &params); err != nil {
			return "", err
		}
		return listDirectory(root, params)
	case ReadFileToolName:
		var params ReadFileParams
		if err := unmarshalParams(paramsJSON, &params); err != nil {
			return "", err
		}
		return readFile(root, params)
	case WriteFileToolName:
		var params WriteFileParams
		if err := unmarshalParams(paramsJSON, &params); err != nil {
			return "", err
		}
		return writeFile(root, params)
	case GlobToolName:
		var params GlobParams
		if err := unmarshalParams(paramsJSON, &params); err != nil {
			return "", err
		}
		return glob(root, params)
	case GrepToolName:
		var params GrepParams
		if err := unmarshalParams(paramsJSON, &params); err != nil {
			return "", err
		}
		return grep(ctx, root, params)
	case ReplaceToolName:
		var params ReplaceParams
		if err := unmarshalParams(paramsJSON, &params); err != nil {
			return "", err
		}
		return replace(root, params)
	case ReadManyFilesToolName:
		var params ReadManyFilesParams
		if err := unmarshalParams(paramsJSON, &params); err != nil {
			return "", err
		}
		return readManyFiles(ctx, root, params)
	default:
		return "", fmt.Errorf("unknown tool %q", toolName)
	}
}

func unmarshalParams(data []byte, into any) error {
	if len(data) == 0 {
		data = []byte("{}")
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("invalid tool parameters: %w", err)
	}
	return nil
}

// resolvePath resolves p against root; absolute paths are used as-is.
// The sandbox provides filesystem isolation, so we don't need to do extra
// path sanitization here.
func resolvePath(root string, p string) string {
	if p == "" {
		return filepath.Clean(root)
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(root, p)
}

// maxLineLength mirrors gemini-cli's MAX_LINE_LENGTH_TEXT_FILE: longer lines
// are cut off to keep tool results bounded.
const maxLineLength = 2000

// truncateLine shortens a single line to maxLineLength characters.
func truncateLine(line string) string {
	runes := []rune(line)
	if len(runes) <= maxLineLength {
		return line
	}
	return string(runes[:maxLineLength]) + "... [truncated]"
}

// isBinary reports whether data looks like binary (non-text) content, using
// the same heuristic as most tools: a NUL byte in the leading bytes.
func isBinary(data []byte) bool {
	n := min(len(data), 8192)
	return bytes.IndexByte(data[:n], 0) >= 0
}
