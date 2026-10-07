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

// ReplaceParams are the arguments of the replace (edit) tool.
type ReplaceParams struct {
	// FilePath is the file to modify.
	FilePath string `json:"file_path"`
	// Instruction describes the intent of the change. It is not used by the
	// implementation, but requiring it (as gemini-cli does) improves edit
	// quality.
	Instruction string `json:"instruction,omitempty"`
	// OldString is the exact literal text to replace. Empty creates a new file.
	OldString string `json:"old_string"`
	// NewString is the exact literal replacement text.
	NewString string `json:"new_string"`
	// AllowMultiple permits replacing every occurrence of OldString.
	AllowMultiple bool `json:"allow_multiple,omitempty"`
}

// replace mirrors gemini-cli's replace tool: an exact-literal-string edit
// that fails loudly when the target text is missing or ambiguous.
func replace(root string, params ReplaceParams) (string, error) {
	if params.FilePath == "" {
		return "", errors.New("file_path is required")
	}
	p := resolvePath(root, params.FilePath)

	// As in gemini-cli, an empty old_string means "create this file".
	if params.OldString == "" {
		if _, err := os.Stat(p); err == nil {
			return "", fmt.Errorf("failed to edit: file already exists and old_string is empty: %s (to overwrite it, use write_file; to edit it, provide the exact text to replace in old_string)", p)
		}
		result, err := writeFile(root, WriteFileParams{FilePath: params.FilePath, Content: params.NewString})
		if err != nil {
			return "", err
		}
		return result, nil
	}

	if params.OldString == params.NewString {
		return "", errors.New("failed to edit: old_string and new_string are identical")
	}

	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("failed to edit, could not read file: %w", err)
	}
	if isBinary(data) {
		return "", fmt.Errorf("cannot edit binary file: %s", p)
	}
	content := string(data)

	count := strings.Count(content, params.OldString)
	if count == 0 {
		return "", fmt.Errorf("failed to edit, could not find the string to replace in %s. The exact text of old_string was not found: check whitespace, indentation, and surrounding context, and re-read the file if needed", p)
	}
	if count > 1 && !params.AllowMultiple {
		return "", fmt.Errorf("failed to edit, expected 1 occurrence of old_string but found %d in %s. Add more surrounding context to old_string to uniquely identify the target, or set allow_multiple to true to replace all occurrences", count, p)
	}

	content = strings.ReplaceAll(content, params.OldString, params.NewString)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write edited file: %w", err)
	}

	if count == 1 {
		return fmt.Sprintf("Successfully modified file: %s (1 replacement).", p), nil
	}
	return fmt.Sprintf("Successfully modified file: %s (%d replacements).", p, count), nil
}
