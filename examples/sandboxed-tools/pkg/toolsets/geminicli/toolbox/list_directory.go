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
	"fmt"
	"os"
	"slices"
	"strings"
)

// ListDirectoryParams are the arguments of the list_directory tool.
type ListDirectoryParams struct {
	// DirPath is the directory to list.
	DirPath string `json:"dir_path"`
	// Ignore holds glob patterns for entries to omit from the listing.
	Ignore []string `json:"ignore,omitempty"`
}

// listDirectory mirrors gemini-cli's list_directory output: directories
// first (marked [DIR]), then files with sizes.
func listDirectory(root string, params ListDirectoryParams) (string, error) {
	dir := resolvePath(root, params.DirPath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("failed to list directory: %w", err)
	}

	type item struct {
		name  string
		isDir bool
		size  int64
	}

	var ignoreMatchers []*GlobMatcher
	for _, pattern := range params.Ignore {
		m, err := NewGlobMatcher(pattern, false)
		if err != nil {
			return "", fmt.Errorf("invalid ignore_pattern %q: %w", pattern, err)
		}
		ignoreMatchers = append(ignoreMatchers, m)
	}

	var items []item
	ignoredCount := 0
	for _, entry := range entries {
		if MatchAnyGlob(ignoreMatchers, entry.Name()) {
			ignoredCount++
			continue
		}
		it := item{name: entry.Name(), isDir: entry.IsDir()}
		if !it.isDir {
			if info, err := entry.Info(); err == nil {
				it.size = info.Size()
			}
		}
		items = append(items, it)
	}

	slices.SortFunc(items, func(a, b item) int {
		if a.isDir != b.isDir {
			if a.isDir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})

	var lines []string
	for _, it := range items {
		if it.isDir {
			lines = append(lines, fmt.Sprintf("[DIR] %s", it.name))
		} else {
			lines = append(lines, fmt.Sprintf("%s (%d bytes)", it.name, it.size))
		}
	}

	result := fmt.Sprintf("Directory listing for %s:\n%s", dir, strings.Join(lines, "\n"))
	if ignoredCount > 0 {
		result += fmt.Sprintf("\n\n(%d ignored)", ignoredCount)
	}
	return result, nil
}
