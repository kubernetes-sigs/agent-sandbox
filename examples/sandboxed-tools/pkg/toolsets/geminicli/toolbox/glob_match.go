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
	"path"
	"strings"
)

// GlobMatcher matches a single glob pattern, and pre-computes the expanded
// glob patterns to avoid re-expanding on every match.
type GlobMatcher struct {
	pattern       string
	caseSensitive bool
	sections      []string
}

// NewGlobMatcher creates a new GlobMatcher.
// In addition to the path.Match syntax within a segment
// ('*', '?', '[...]'), it supports '**' (matches zero or more path segments)
// and brace alternation ('{a,b}'), which gemini-cli's tool descriptions
// advertise (e.g. "src/**/*.{ts,tsx}").
func NewGlobMatcher(pattern string, caseSensitive bool) (*GlobMatcher, error) {
	m := &GlobMatcher{
		pattern:       pattern,
		caseSensitive: caseSensitive,
	}

	if !caseSensitive {
		pattern = strings.ToLower(pattern)
	}

	m.sections = expandBraces(pattern)

	// Verify the segments
	for _, expanded := range m.sections {
		for segment := range strings.SplitSeq(expanded, "/") {
			if _, err := path.Match(segment, "test"); err != nil {
				return nil, fmt.Errorf("path.Match %q failed: %v", segment, err)
			}
		}
	}

	return m, nil
}

// Matches checks whether the slash-separated relative path p matches the
// glob pattern.
func (m *GlobMatcher) Matches(p string) bool {
	if !m.caseSensitive {
		p = strings.ToLower(p)
	}

	// Split the path into segments and compare with the pattern segments
	for _, expanded := range m.sections {
		if matchSegments(strings.Split(expanded, "/"), strings.Split(p, "/")) {
			return true
		}
	}
	return false
}

// MatchAnyGlob reports whether p matches any of the given matchers.
func MatchAnyGlob(matchers []*GlobMatcher, p string) bool {
	for _, m := range matchers {
		if m.Matches(p) {
			return true
		}
	}
	return false
}

func matchSegments(pattern []string, segments []string) bool {
	if len(pattern) == 0 {
		return len(segments) == 0
	}
	if pattern[0] == "**" {
		// '**' matches zero or more whole path segments.
		for i := 0; i <= len(segments); i++ {
			if matchSegments(pattern[1:], segments[i:]) {
				return true
			}
		}
		return false
	}
	if len(segments) == 0 {
		return false
	}
	ok, err := path.Match(pattern[0], segments[0])
	if err != nil {
		// This should not happen - pattern should have been checked above
		panic(fmt.Sprintf("path.Match %q failed: %v", pattern[0], err))
	}
	if !ok {
		return false
	}
	return matchSegments(pattern[1:], segments[1:])
}

// expandBraces expands the first brace alternation in the pattern and recurses,
// turning "a.{b,c}" into ["a.b", "a.c"]. Patterns without braces are returned
// unchanged. Nested braces are supported; unbalanced braces are left literal.
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}

	depth := 0
	for i := open; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var out []string
				for _, alt := range splitAlternatives(pattern[open+1 : i]) {
					out = append(out, expandBraces(pattern[:open]+alt+pattern[i+1:])...)
				}
				return out
			}
		}
	}
	// Unbalanced '{': treat as a literal.
	return []string{pattern}
}

// splitAlternatives splits a brace body on top-level commas ("b,{c,d}e" =>
// ["b", "{c,d}e"]).
func splitAlternatives(body string) []string {
	var alternatives []string
	depth := 0
	start := 0
	for i := range len(body) {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				alternatives = append(alternatives, body[start:i])
				start = i + 1
			}
		}
	}
	return append(alternatives, body[start:])
}
