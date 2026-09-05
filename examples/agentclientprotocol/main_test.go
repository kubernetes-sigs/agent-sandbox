/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/agent-sandbox/examples/agentclientprotocol/pkg/acp"
)

func newTestConsole(t *testing.T) (*console, string) {
	t.Helper()
	workDir := t.TempDir()
	// Match run(): the console compares against a symlink-resolved workDir.
	workDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", workDir, err)
	}
	// Construct the console directly rather than via newConsole, so the
	// test does not start the view goroutine that owns terminal I/O.
	return &console{workDir: workDir}, workDir
}

func TestResolvePath(t *testing.T) {
	cons, workDir := newTestConsole(t)

	grandparent := filepath.Dir(filepath.Dir(workDir))

	// Layout: an existing file and subdirectory inside the working
	// directory, plus symlinks pointing both inside and outside of it.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "target.txt")
	for _, f := range []string{outsideFile, filepath.Join(workDir, "real.txt")} {
		if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(workDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinks := map[string]string{
		"link-outside": outsideFile,                        // file symlink escaping workDir
		"dir-outside":  outside,                            // directory symlink escaping workDir
		"link-inside":  filepath.Join(workDir, "real.txt"), // symlink staying inside workDir
	}
	for name, target := range symlinks {
		if err := os.Symlink(target, filepath.Join(workDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "relative inside", path: "sub/file.txt", want: filepath.Join(workDir, "sub", "file.txt")},
		{name: "absolute inside", path: filepath.Join(workDir, "file.txt"), want: filepath.Join(workDir, "file.txt")},
		{name: "dot", path: ".", want: workDir},
		{name: "existing file", path: "real.txt", want: filepath.Join(workDir, "real.txt")},
		{name: "relative traversal", path: "../escape.txt", wantErr: true},
		{name: "nested traversal", path: "sub/../../escape.txt", wantErr: true},
		{name: "absolute outside", path: filepath.Join(grandparent, "escape.txt"), wantErr: true},
		{name: "parent directory itself", path: "..", wantErr: true},
		{name: "nonexistent parent", path: "no-such-dir/file.txt", wantErr: true},
		{name: "symlink file escaping", path: "link-outside", wantErr: true},
		{name: "symlink dir escaping", path: "dir-outside/target.txt", wantErr: true},
		{name: "symlink staying inside", path: "link-inside", want: filepath.Join(workDir, "real.txt")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cons.resolvePath(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolvePath(%q) = %q, want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePath(%q): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("resolvePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestReadTextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour"), 0o644); err != nil {
		t.Fatal(err)
	}

	intp := func(n int) *int { return &n }

	tests := []struct {
		name  string
		line  *int
		limit *int
		want  string
	}{
		{name: "whole file", want: "one\ntwo\nthree\nfour"},
		{name: "from line 2", line: intp(2), want: "two\nthree\nfour"},
		{name: "line and limit", line: intp(2), limit: intp(2), want: "two\nthree"},
		{name: "limit only", limit: intp(1), want: "one"},
		{name: "line past end", line: intp(10), want: ""},
		{name: "limit past end", line: intp(3), limit: intp(10), want: "three\nfour"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readTextFile(acp.ReadTextFileParams{Path: path, Line: tc.line, Limit: tc.limit})
			if err != nil {
				t.Fatalf("readTextFile: %v", err)
			}
			if got != tc.want {
				t.Errorf("readTextFile(line=%v, limit=%v) = %q, want %q", tc.line, tc.limit, got, tc.want)
			}
		})
	}
}
