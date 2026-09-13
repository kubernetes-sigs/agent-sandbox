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
	"path/filepath"
)

// WriteFileParams are the arguments of the write_file tool.
type WriteFileParams struct {
	// FilePath is the file to write.
	FilePath string `json:"file_path"`
	// Content is the full content to write.
	Content string `json:"content"`
}

// writeFile mirrors gemini-cli's write_file: writes the full content,
// creating parent directories as needed.
func writeFile(root string, params WriteFileParams) (string, error) {
	if params.FilePath == "" {
		return "", errors.New("file_path is required")
	}
	p := resolvePath(root, params.FilePath)

	existed := false
	if info, err := os.Stat(p); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("path is a directory, not a file: %s", p)
		}
		existed = true
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("failed to create parent directories: %w", err)
	}
	if err := os.WriteFile(p, []byte(params.Content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	if existed {
		return fmt.Sprintf("Successfully overwrote file: %s", p), nil
	}
	return fmt.Sprintf("Successfully created and wrote to new file: %s", p), nil
}
