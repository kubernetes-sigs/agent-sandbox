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

package toolbox

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ReadFileParams are the arguments of the read_file tool.
type ReadFileParams struct {
	// FilePath is the file to read.
	FilePath string `json:"file_path"`
	// StartLine is the optional 1-based first line to read.
	StartLine int `json:"start_line,omitempty"`
	// EndLine is the optional 1-based last line to read (inclusive).
	EndLine int `json:"end_line,omitempty"`
}

// maxLinesPerRead mirrors gemini-cli's DEFAULT_MAX_LINES_TEXT_FILE.
const maxLinesPerRead = 2000

// maxReadFileSize bounds how much of a file we are willing to load.
const maxReadFileSize = 20 * 1024 * 1024

// readFile mirrors gemini-cli's read_file: text content with optional line
// ranges, truncated with an explanatory header when the file is large.
func readFile(root string, params ReadFileParams) (string, error) {
	if params.FilePath == "" {
		return "", errors.New("file_path is required")
	}
	p := resolvePath(root, params.FilePath)

	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file: %s (use list_directory instead)", p)
	}
	if info.Size() > maxReadFileSize {
		return "", fmt.Errorf("file %s is too large to read (%d bytes)", p, info.Size())
	}

	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	if isBinary(data) {
		return "", fmt.Errorf("cannot display content of binary file: %s", p)
	}

	lines := strings.Split(string(data), "\n")
	totalLines := len(lines)

	start := 1
	if params.StartLine > 0 {
		start = params.StartLine
	}
	if start > totalLines {
		return "", fmt.Errorf("start_line %d is beyond the end of the file (%d lines)", start, totalLines)
	}
	end := min(start+maxLinesPerRead-1, totalLines)
	if params.EndLine > 0 {
		end = min(params.EndLine, end)
	}
	if end < start {
		return "", fmt.Errorf("end_line %d is before start_line %d", params.EndLine, start)
	}

	selected := lines[start-1 : end]
	for i, line := range selected {
		selected[i] = truncateLine(line)
	}
	content := strings.Join(selected, "\n")

	if start > 1 || end < totalLines {
		return fmt.Sprintf(`
IMPORTANT: The file content has been truncated.
Status: Showing lines %d-%d of %d total lines.
Action: To read more of the file, you can use the 'start_line' and 'end_line' parameters in a subsequent 'read_file' call. For example, to read the next section of the file, use start_line: %d.

--- FILE CONTENT (truncated) ---
%s`, start, end, totalLines, end+1, content), nil
	}
	return content, nil
}
