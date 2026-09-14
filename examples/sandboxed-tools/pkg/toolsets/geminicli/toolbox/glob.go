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
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// GlobParams are the arguments of the glob tool.
type GlobParams struct {
	// Pattern is the glob pattern to match, e.g. "src/**/*.go".
	Pattern string `json:"pattern"`
	// DirPath is the directory to search in (defaults to the workspace root).
	DirPath string `json:"dir_path,omitempty"`
	// CaseSensitive makes matching case-sensitive (default false).
	CaseSensitive bool `json:"case_sensitive,omitempty"`
}

// alwaysSkippedDirs are directories never traversed by glob/grep/read_many_files:
// they are effectively never what the model is looking for, and walking them
// is slow and noisy. This stands in for gemini-cli's gitignore handling.
//
// TODO: gemini-cli respects .gitignore and .geminiignore when searching and
// listing (with respect_git_ignore / respect_gemini_ignore / no_ignore /
// file_filtering_options parameters on glob, grep_search, list_directory,
// and read_many_files). Parsing those ignore files here would tighten
// results in real repositories; the parameters are omitted from our schemas
// until the behavior exists.
var alwaysSkippedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
}

// glob mirrors gemini-cli's glob tool: matching file paths, newest first.
func glob(root string, params GlobParams) (string, error) {
	if params.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	dir := resolvePath(root, params.DirPath)

	type match struct {
		path    string
		modTime time.Time
	}
	var matches []match

	matcher, err := NewGlobMatcher(params.Pattern, params.CaseSensitive)
	if err != nil {
		return "", fmt.Errorf("invalid glob pattern %q: %w", params.Pattern, err)
	}

	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Report unreadable roots, skip unreadable subtrees.
			if p == dir {
				return err
			}
			return nil
		}
		if d.IsDir() {
			if p != dir && alwaysSkippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		if !matcher.Matches(filepath.ToSlash(rel)) {
			return nil
		}
		m := match{path: p}
		if info, err := d.Info(); err == nil {
			m.modTime = info.ModTime()
		}
		matches = append(matches, m)
		return nil
	}); err != nil {
		return "", fmt.Errorf("failed to search %s: %w", dir, err)
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No files found matching pattern %q within %s.", params.Pattern, dir), nil
	}

	slices.SortFunc(matches, func(a, b match) int {
		if !a.modTime.Equal(b.modTime) {
			if a.modTime.After(b.modTime) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.path, b.path)
	})

	var paths []string
	for _, m := range matches {
		paths = append(paths, m.path)
	}
	return fmt.Sprintf("Found %d file(s) matching %q within %s, sorted by modification time (newest first):\n%s",
		len(matches), params.Pattern, dir, strings.Join(paths, "\n")), nil
}
