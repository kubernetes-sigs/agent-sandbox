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

import "testing"

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern       string
		path          string
		caseSensitive bool
		want          bool
	}{
		{pattern: "*.md", path: "README.md", want: true},
		{pattern: "*.md", path: "docs/README.md", want: false},
		{pattern: "**/*.md", path: "docs/README.md", want: true},
		{pattern: "**/*.md", path: "README.md", want: true},
		{pattern: "**/*.md", path: "a/b/c/README.md", want: true},
		{pattern: "src/**/*.go", path: "src/pkg/deep/main.go", want: true},
		{pattern: "src/**/*.go", path: "other/pkg/main.go", want: false},
		{pattern: "src/**", path: "src/anything/at/all.txt", want: true},
		{pattern: "src/**", path: "srcx/file.txt", want: false},
		{pattern: "*.{ts,tsx}", path: "app.tsx", want: true},
		{pattern: "*.{ts,tsx}", path: "app.ts", want: true},
		{pattern: "*.{ts,tsx}", path: "app.go", want: false},
		{pattern: "file?.txt", path: "file1.txt", want: true},
		{pattern: "file?.txt", path: "file12.txt", want: false},
		{pattern: "README.MD", path: "readme.md", want: true},
		{pattern: "README.MD", path: "readme.md", caseSensitive: true, want: false},
		{pattern: "a/{b,c/d}/e.txt", path: "a/c/d/e.txt", want: true},
		{pattern: "a/{b,c/d}/e.txt", path: "a/b/e.txt", want: true},
		// Unbalanced braces are treated as literals.
		{pattern: "a{b.txt", path: "a{b.txt", want: true},
	}
	for _, tc := range tests {
		m, err := NewGlobMatcher(tc.pattern, tc.caseSensitive)
		if err != nil {
			t.Errorf("NewGlobMatcher(%q, caseSensitive=%v) failed: %v", tc.pattern, tc.caseSensitive, err)
			continue
		}
		if got := m.Matches(tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q, caseSensitive=%v) = %v, want %v", tc.pattern, tc.path, tc.caseSensitive, got, tc.want)
		}
	}
}
