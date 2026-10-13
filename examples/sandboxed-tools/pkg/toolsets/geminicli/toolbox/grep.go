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
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/agent-sandbox/examples/sandboxed-tools/pkg/tools"
)

// GrepParams are the arguments of the grep_search tool.
type GrepParams struct {
	// Pattern is the regular expression to search for.
	Pattern string `json:"pattern"`
	// DirPath is the directory (or single file) to search (defaults to the
	// workspace root). Directories are searched recursively.
	DirPath string `json:"dir_path,omitempty"`
	// IncludePattern is a glob restricting which files are searched.
	// Ignored if DirPath is a single file.
	IncludePattern string `json:"include_pattern,omitempty"`
	// ExcludePattern is a regex; matching lines are omitted from the results.
	// Ignored if DirPath is a single file.
	ExcludePattern string `json:"exclude_pattern,omitempty"`
	// NamesOnly returns only the matching file paths.
	NamesOnly bool `json:"names_only,omitempty"`
	// CaseSensitive makes the search case-sensitive (default false).
	CaseSensitive bool `json:"case_sensitive,omitempty"`
	// FixedStrings treats Pattern as a literal string.
	FixedStrings bool `json:"fixed_strings,omitempty"`
	// Context requests this many lines of context around each match (-C).
	Context int `json:"context,omitempty"`
	// After requests this many lines after each match (-A).
	After int `json:"after,omitempty"`
	// Before requests this many lines before each match (-B).
	Before int `json:"before,omitempty"`
	// MaxMatchesPerFile bounds matches per file.
	MaxMatchesPerFile int `json:"max_matches_per_file,omitempty"`
	// TotalMaxMatches bounds total matches (default 100).
	TotalMaxMatches int `json:"total_max_matches,omitempty"`
}

// defaultTotalMaxMatches mirrors gemini-cli's default cap of 100 matches.
const defaultTotalMaxMatches = 100

// maxGrepFileSize bounds the size of files we search.
const maxGrepFileSize = 10 * 1024 * 1024

// maxGrepResponseSize bounds the total response size.
const maxGrepResponseSize = 4 * 1024 * 1024

// grepLine is one output line: either a match or surrounding context.
type grepLine struct {
	number    int
	text      string
	isContext bool
}

// grep mirrors gemini-cli's grep_search tool using Go's regexp engine.
func grep(ctx context.Context, root string, params GrepParams) (string, error) {
	if params.Pattern == "" {
		return "", errors.New("pattern is required")
	}

	expr := params.Pattern
	if params.FixedStrings {
		expr = regexp.QuoteMeta(expr)
	}
	if !params.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid regular expression pattern %q: %w", params.Pattern, err)
	}

	var exclude *regexp.Regexp
	if params.ExcludePattern != "" {
		exclude, err = regexp.Compile(params.ExcludePattern)
		if err != nil {
			return "", fmt.Errorf("invalid exclude_pattern %q: %w", params.ExcludePattern, err)
		}
	}

	totalMax := params.TotalMaxMatches
	if totalMax <= 0 {
		totalMax = defaultTotalMaxMatches
	}
	// cap total max matches at 1024 to avoid overwhelming the model
	if totalMax > 1024 {
		return "", errors.New("total_max_matches too large: 1024 is the maximum allowed")
	}

	before := max(params.Before, params.Context)
	after := max(params.After, params.Context)

	// cap before and after at 1024 to avoid overwhelming the model
	if before > 1024 {
		return "", errors.New("before context too large: 1024 is the maximum allowed")
	}
	if after > 1024 {
		return "", errors.New("after context too large: 1024 is the maximum allowed")
	}

	target := resolvePath(root, params.DirPath)
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("failed to search %s: %w", target, err)
	}

	var globMatcher *GlobMatcher
	if params.IncludePattern != "" {
		globMatcher, err = NewGlobMatcher(params.IncludePattern, true)
		if err != nil {
			return "", fmt.Errorf("invalid include_pattern %q: %w", params.IncludePattern, err)
		}
	}

	// Collect the files to search, in a deterministic order.
	var files []string
	if !info.IsDir() {
		// If the target is a single file, just search that file.
		// We don't apply includePattern etc
		files = append(files, target)
	} else {
		walkErr := filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == target {
					return err
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if p != target && alwaysSkippedDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if params.IncludePattern != "" {
				rel, err := filepath.Rel(target, p)
				if err != nil {
					// This should not happen, so if it does just bail out.
					return fmt.Errorf("failed to compute relative path for %s: %w", p, err)
				}
				// Like ripgrep's -g, a basename pattern such as "*.go"
				// matches at any depth.
				if !globMatcher.Matches(filepath.ToSlash(rel)) &&
					!globMatcher.Matches(d.Name()) {
					return nil
				}
			}
			files = append(files, p)
			return nil
		})
		if walkErr != nil {
			return "", fmt.Errorf("failed to search %s: %w", target, walkErr)
		}
		slices.Sort(files)
	}

	matchesByFile := make(map[string][]grepLine)
	var matchedFiles []string
	totalMatches := 0
	truncated := false

fileLoop:
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		if info, err := os.Stat(file); err != nil || info.Size() > maxGrepFileSize {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil || isBinary(data) {
			continue
		}

		lines := strings.Split(string(data), "\n")
		matchesInFile := 0
		var kept []grepLine
		keptLines := make(map[int]bool)

		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			if exclude != nil && exclude.MatchString(line) {
				continue
			}
			if params.MaxMatchesPerFile > 0 && matchesInFile >= params.MaxMatchesPerFile {
				break
			}

			// Insert any context lines before the match that we haven't
			// already emitted.
			for j := max(0, i-before); j < i; j++ {
				if !keptLines[j] {
					kept = append(kept, grepLine{number: j + 1, text: lines[j], isContext: true})
					keptLines[j] = true
				}
			}
			if keptLines[i] {
				// Previously emitted as context; upgrade it to a match.
				for k := range kept {
					if kept[k].number == i+1 {
						kept[k].isContext = false
					}
				}
			} else {
				kept = append(kept, grepLine{number: i + 1, text: line})
				keptLines[i] = true
			}
			for j := i + 1; j <= min(len(lines)-1, i+after); j++ {
				if !keptLines[j] {
					kept = append(kept, grepLine{number: j + 1, text: lines[j], isContext: true})
					keptLines[j] = true
				}
			}

			matchesInFile++
			totalMatches++
			if totalMatches >= totalMax {
				truncated = true
				if len(kept) > 0 {
					matchesByFile[file] = kept
					matchedFiles = append(matchedFiles, file)
				}
				break fileLoop
			}
		}

		if len(kept) > 0 {
			matchesByFile[file] = kept
			matchedFiles = append(matchedFiles, file)
		}
	}

	searchLocation := fmt.Sprintf("in %s", target)
	filterSuffix := ""
	if params.IncludePattern != "" {
		filterSuffix = fmt.Sprintf(" (filter: %q)", params.IncludePattern)
	}

	if totalMatches == 0 {
		return fmt.Sprintf("No matches found for pattern %q %s%s.", params.Pattern, searchLocation, filterSuffix), nil
	}

	truncationSuffix := ""
	if truncated {
		truncationSuffix = fmt.Sprintf(" (results limited to %d matches for performance)", totalMax)
	}

	if params.NamesOnly {
		return fmt.Sprintf("Found %d files with matches for pattern %q %s%s%s:\n%s",
			len(matchedFiles), params.Pattern, searchLocation, filterSuffix, truncationSuffix,
			strings.Join(matchedFiles, "\n")), nil
	}

	matchTerm := "matches"
	if totalMatches == 1 {
		matchTerm = "match"
	}
	out := tools.NewLimitedWriter(maxGrepResponseSize)

	fmt.Fprintf(out, "Found %d %s for pattern %q %s%s%s:\n---\n",
		totalMatches, matchTerm, params.Pattern, searchLocation, filterSuffix, truncationSuffix)
	for _, file := range matchedFiles {
		fmt.Fprintf(out, "File: %s\n", file)
		for _, line := range matchesByFile[file] {
			separator := ":"
			if line.isContext {
				separator = "-"
			}
			fmt.Fprintf(out, "L%d%s %s\n", line.number, separator, truncateLine(strings.TrimRight(line.text, " \t\r")))
		}
		fmt.Fprintf(out, "---\n")
	}

	output := out.String()

	if out.Truncated() {
		output += fmt.Sprintf("\n[Output truncated at %d bytes]", out.Len())
	}

	return output, nil
}
