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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ReadManyFilesParams are the arguments of the read_many_files tool.
type ReadManyFilesParams struct {
	// Include holds glob patterns or paths to read.
	Include []string `json:"include"`
	// Exclude holds glob patterns to skip (added to the default excludes).
	Exclude []string `json:"exclude,omitempty"`
	// Recursive is accepted for schema compatibility; recursion is controlled
	// by '**' in the glob patterns.
	Recursive *bool `json:"recursive,omitempty"`
	// UseDefaultExcludes toggles the built-in exclude list (default true).
	// Note that we always skip alwaysSkippedDirs
	UseDefaultExcludes *bool `json:"useDefaultExcludes,omitempty"`
}

// defaultExcludePatterns is a trimmed-down version of gemini-cli's default
// exclude list, covering the directories and file types that most often
// pollute bulk reads.
var defaultExcludePatterns = []string{
	"**/node_modules/**",
	"**/.git/**",
	"**/dist/**",
	"**/build/**",
	"**/coverage/**",
	"**/__pycache__/**",
	"**/*.min.js",
	"**/*.min.css",
	"**/.DS_Store",
}

// maxReadManyFileSize bounds each individual file included in the output.
const maxReadManyFileSize = 1024 * 1024

// maxReadManyTotalSize bounds the total output size.
const maxReadManyTotalSize = 8 * 1024 * 1024

// readManyFiles mirrors gemini-cli's read_many_files: concatenates the
// content of every text file matching the include patterns.
//
// TODO: gemini-cli also includes image/audio/PDF files as base64 parts when
// they are explicitly named in 'include'; we skip all binary files (they are
// listed in the "Skipped" section of the result instead).
func readManyFiles(ctx context.Context, root string, params ReadManyFilesParams) (string, error) {
	if len(params.Include) == 0 {
		return "", errors.New("include is required and must contain at least one pattern")
	}

	// Normalize include entries: a directory path means "everything under it".
	var includes []string
	for _, entry := range params.Include {
		// Note this logic doesn't handle . and ./docs well, but we want to be compatible with gemini-cli
		entry = filepath.ToSlash(strings.TrimSuffix(entry, "/"))
		if entry == "" {
			continue
		}
		if info, err := os.Stat(resolvePath(root, entry)); err == nil && info.IsDir() {
			entry = entry + "/**"
		}
		includes = append(includes, entry)
	}

	excludes := slices.Clone(params.Exclude)
	if params.UseDefaultExcludes == nil || *params.UseDefaultExcludes {
		excludes = append(excludes, defaultExcludePatterns...)
	}
	var includeMatchers []*GlobMatcher
	for _, pattern := range includes {
		m, err := NewGlobMatcher(pattern, false)
		if err != nil {
			return "", fmt.Errorf("invalid include_pattern %q: %w", pattern, err)
		}
		includeMatchers = append(includeMatchers, m)
	}
	var excludeMatchers []*GlobMatcher
	for _, pattern := range excludes {
		m, err := NewGlobMatcher(pattern, false)
		if err != nil {
			return "", fmt.Errorf("invalid exclude_pattern %q: %w", pattern, err)
		}
		excludeMatchers = append(excludeMatchers, m)
	}

	var selected []string
	var skipped []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != root && alwaysSkippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			// This should not happen, so if it does just bail out.
			return fmt.Errorf("failed to compute relative path for %s: %w", p, err)
		}
		relSlash := filepath.ToSlash(rel)
		if !MatchAnyGlob(includeMatchers, relSlash) {
			return nil
		}
		if MatchAnyGlob(excludeMatchers, relSlash) {
			return nil
		}
		selected = append(selected, p)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to search %s: %w", root, err)
	}
	slices.Sort(selected)

	var sb strings.Builder
	readCount := 0
	for _, p := range selected {
		if sb.Len() >= maxReadManyTotalSize {
			skipped = append(skipped, fmt.Sprintf("%s (total output size limit reached)", p))
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", p, err))
			continue
		}
		if info.Size() > maxReadManyFileSize {
			skipped = append(skipped, fmt.Sprintf("%s (file too large: %d bytes)", p, info.Size()))
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", p, err))
			continue
		}
		if isBinary(data) {
			skipped = append(skipped, fmt.Sprintf("%s (binary file)", p))
			continue
		}

		estimatedAdd := len(data) + len(p) + 32 //rough estimate
		if sb.Len()+estimatedAdd > maxReadManyTotalSize {
			skipped = append(skipped, fmt.Sprintf("%s (total output size limit reached)", p))
			continue
		}

		fmt.Fprintf(&sb, "--- %s ---\n\n%s\n\n", p, string(data))
		readCount++
	}

	if readCount == 0 && len(skipped) == 0 {
		return "No files matching the criteria were found.", nil
	}

	// Note: this might result in more than the maxReadManyTotalSize output,
	// but we are mirroring gemini-cli behaviour.
	sb.WriteString("--- End of content ---")
	if len(skipped) > 0 {
		fmt.Fprintf(&sb, "\n\nSkipped %d file(s):\n%s", len(skipped), strings.Join(skipped, "\n"))
	}
	return sb.String(), nil
}
