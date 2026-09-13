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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestFile creates a file (and its parent directories) under root.
func writeTestFile(t *testing.T, root string, rel string, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// runTool invokes Run with params marshalled to JSON, like the real binary.
func runTool(t *testing.T, root string, tool string, params any) (string, error) {
	t.Helper()
	payload, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return Run(t.Context(), root, tool, payload)
}

func TestRunUnknownTool(t *testing.T) {
	if _, err := Run(t.Context(), t.TempDir(), "no_such_tool", []byte("{}")); err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestListDirectory(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "b.txt", "hello")
	writeTestFile(t, root, "a.log", "log")
	writeTestFile(t, root, "sub/child.txt", "child")

	got, err := runTool(t, root, ListDirectoryToolName, ListDirectoryParams{DirPath: "."})
	if err != nil {
		t.Fatalf("listDirectory: %v", err)
	}
	want := fmt.Sprintf("Directory listing for %s:\n[DIR] sub\na.log (3 bytes)\nb.txt (5 bytes)", root)
	if got != want {
		t.Errorf("listDirectory = %q, want %q", got, want)
	}

	// Ignore patterns are applied to entry names and reported.
	got, err = runTool(t, root, ListDirectoryToolName, ListDirectoryParams{DirPath: ".", Ignore: []string{"*.log"}})
	if err != nil {
		t.Fatalf("listDirectory with ignore: %v", err)
	}
	if strings.Contains(got, "a.log") || !strings.Contains(got, "(1 ignored)") {
		t.Errorf("listDirectory with ignore = %q, want a.log ignored", got)
	}

	if _, err := runTool(t, root, ListDirectoryToolName, ListDirectoryParams{DirPath: "missing"}); err == nil {
		t.Error("expected error for missing directory")
	}
}

func TestReadFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "hello.txt", "line1\nline2\nline3\nline4\nline5")

	got, err := runTool(t, root, ReadFileToolName, ReadFileParams{FilePath: "hello.txt"})
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if got != "line1\nline2\nline3\nline4\nline5" {
		t.Errorf("readFile = %q", got)
	}

	// A line range triggers the truncation header with resume instructions.
	got, err = runTool(t, root, ReadFileToolName, ReadFileParams{FilePath: "hello.txt", StartLine: 2, EndLine: 3})
	if err != nil {
		t.Fatalf("readFile range: %v", err)
	}
	if !strings.Contains(got, "Showing lines 2-3 of 5 total lines") {
		t.Errorf("readFile range missing truncation status: %q", got)
	}
	if !strings.Contains(got, "start_line: 4") {
		t.Errorf("readFile range missing resume hint: %q", got)
	}
	if !strings.Contains(got, "line2\nline3") {
		t.Errorf("readFile range missing content: %q", got)
	}

	if _, err := runTool(t, root, ReadFileToolName, ReadFileParams{FilePath: "missing.txt"}); err == nil {
		t.Error("expected error for missing file")
	}
	if _, err := runTool(t, root, ReadFileToolName, ReadFileParams{}); err == nil {
		t.Error("expected error for missing file_path")
	}

	writeTestFile(t, root, "binary.bin", "abc\x00def")
	if _, err := runTool(t, root, ReadFileToolName, ReadFileParams{FilePath: "binary.bin"}); err == nil {
		t.Error("expected error for binary file")
	}
}

func TestReadFileTruncatesLongFiles(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= maxLinesPerRead+100; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	writeTestFile(t, root, "big.txt", sb.String())

	got, err := runTool(t, root, ReadFileToolName, ReadFileParams{FilePath: "big.txt"})
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if !strings.Contains(got, "IMPORTANT: The file content has been truncated.") {
		t.Errorf("expected truncation banner, got prefix %q", got[:100])
	}
	if !strings.Contains(got, fmt.Sprintf("Showing lines 1-%d", maxLinesPerRead)) {
		t.Errorf("expected default line cap of %d, got prefix %q", maxLinesPerRead, got[:200])
	}
}

func TestWriteFile(t *testing.T) {
	root := t.TempDir()

	got, err := runTool(t, root, WriteFileToolName, WriteFileParams{FilePath: "sub/dir/new.txt", Content: "hello"})
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if !strings.Contains(got, "Successfully created and wrote to new file") {
		t.Errorf("writeFile = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "sub/dir/new.txt"))
	if err != nil || string(data) != "hello" {
		t.Errorf("file content = %q, err = %v", data, err)
	}

	got, err = runTool(t, root, WriteFileToolName, WriteFileParams{FilePath: "sub/dir/new.txt", Content: "changed"})
	if err != nil {
		t.Fatalf("writeFile overwrite: %v", err)
	}
	if !strings.Contains(got, "Successfully overwrote file") {
		t.Errorf("writeFile overwrite = %q", got)
	}
}

func TestReplace(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "code.go", "func a() {}\nfunc b() {}\nfunc b2() {}\n")

	// Unique replacement succeeds.
	got, err := runTool(t, root, ReplaceToolName, ReplaceParams{
		FilePath:  "code.go",
		OldString: "func a() {}",
		NewString: "func a() { return }",
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !strings.Contains(got, "(1 replacement)") {
		t.Errorf("replace = %q", got)
	}
	data, _ := os.ReadFile(filepath.Join(root, "code.go"))
	if !strings.Contains(string(data), "func a() { return }") {
		t.Errorf("file after replace = %q", data)
	}

	// Ambiguous old_string fails without allow_multiple...
	if _, err := runTool(t, root, ReplaceToolName, ReplaceParams{
		FilePath:  "code.go",
		OldString: "func b",
		NewString: "func c",
	}); err == nil || !strings.Contains(err.Error(), "found 2") {
		t.Errorf("expected ambiguity error, got %v", err)
	}

	// ...and succeeds with it.
	got, err = runTool(t, root, ReplaceToolName, ReplaceParams{
		FilePath:      "code.go",
		OldString:     "func b",
		NewString:     "func c",
		AllowMultiple: true,
	})
	if err != nil {
		t.Fatalf("replace allow_multiple: %v", err)
	}
	if !strings.Contains(got, "(2 replacements)") {
		t.Errorf("replace allow_multiple = %q", got)
	}

	// Missing old_string fails.
	if _, err := runTool(t, root, ReplaceToolName, ReplaceParams{
		FilePath:  "code.go",
		OldString: "does not exist",
		NewString: "x",
	}); err == nil {
		t.Error("expected error for old_string not found")
	}

	// Identical old/new fails.
	if _, err := runTool(t, root, ReplaceToolName, ReplaceParams{
		FilePath:  "code.go",
		OldString: "same",
		NewString: "same",
	}); err == nil {
		t.Error("expected error for identical old_string and new_string")
	}

	// Empty old_string creates a new file, but refuses to clobber.
	if _, err := runTool(t, root, ReplaceToolName, ReplaceParams{
		FilePath:  "fresh.txt",
		NewString: "created",
	}); err != nil {
		t.Fatalf("replace create: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "fresh.txt"))
	if string(data) != "created" {
		t.Errorf("created file content = %q", data)
	}
	if _, err := runTool(t, root, ReplaceToolName, ReplaceParams{
		FilePath:  "fresh.txt",
		NewString: "clobbered",
	}); err == nil {
		t.Error("expected error creating file that already exists")
	}
}

func TestGlob(t *testing.T) {
	root := t.TempDir()
	oldFile := writeTestFile(t, root, "old.go", "old")
	writeTestFile(t, root, "sub/new.go", "new")
	writeTestFile(t, root, "sub/other.txt", "other")
	writeTestFile(t, root, ".git/config.go", "should be skipped")

	// Make mtimes deterministic: old.go is older.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	got, err := runTool(t, root, GlobToolName, GlobParams{Pattern: "**/*.go"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(got, "Found 2 file(s)") {
		t.Errorf("glob = %q", got)
	}
	newIdx := strings.Index(got, filepath.Join(root, "sub/new.go"))
	oldIdx := strings.Index(got, filepath.Join(root, "old.go"))
	if newIdx < 0 || oldIdx < 0 || newIdx > oldIdx {
		t.Errorf("glob should list newest first: %q", got)
	}
	if strings.Contains(got, ".git") {
		t.Errorf("glob should skip .git: %q", got)
	}

	got, err = runTool(t, root, GlobToolName, GlobParams{Pattern: "*.missing"})
	if err != nil {
		t.Fatalf("glob no match: %v", err)
	}
	if !strings.HasPrefix(got, "No files found matching pattern") {
		t.Errorf("glob no match = %q", got)
	}
}

func TestGrep(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.go", "package main\n\nfunc Hello() {}\n")
	writeTestFile(t, root, "b.go", "package main\n\nfunc hello2() {}\n")
	writeTestFile(t, root, "c.txt", "hello text\n")

	// Case-insensitive by default; include_pattern filters files.
	got, err := runTool(t, root, GrepToolName, GrepParams{Pattern: "hello", IncludePattern: "*.go"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(got, "Found 2 matches") {
		t.Errorf("grep = %q", got)
	}
	if !strings.Contains(got, "L3: func Hello() {}") {
		t.Errorf("grep missing match line: %q", got)
	}
	if strings.Contains(got, "c.txt") {
		t.Errorf("grep include_pattern should exclude c.txt: %q", got)
	}

	// Case-sensitive narrows to one.
	got, err = runTool(t, root, GrepToolName, GrepParams{Pattern: "Hello", CaseSensitive: true})
	if err != nil {
		t.Fatalf("grep case-sensitive: %v", err)
	}
	if !strings.Contains(got, "Found 1 match ") {
		t.Errorf("grep case-sensitive = %q", got)
	}

	// names_only lists file paths.
	got, err = runTool(t, root, GrepToolName, GrepParams{Pattern: "hello", NamesOnly: true})
	if err != nil {
		t.Fatalf("grep names_only: %v", err)
	}
	if !strings.Contains(got, "Found 3 files with matches") {
		t.Errorf("grep names_only = %q", got)
	}
	if strings.Contains(got, "L3") {
		t.Errorf("grep names_only should not include line numbers: %q", got)
	}

	// Context lines use '-' separators.
	got, err = runTool(t, root, GrepToolName, GrepParams{Pattern: "func Hello", Context: 1})
	if err != nil {
		t.Fatalf("grep context: %v", err)
	}
	if !strings.Contains(got, "L2-") || !strings.Contains(got, "L3: func Hello() {}") {
		t.Errorf("grep context = %q", got)
	}

	// No matches.
	got, err = runTool(t, root, GrepToolName, GrepParams{Pattern: "zzznope"})
	if err != nil {
		t.Fatalf("grep no match: %v", err)
	}
	if !strings.HasPrefix(got, "No matches found") {
		t.Errorf("grep no match = %q", got)
	}

	// total_max_matches truncates and says so.
	got, err = runTool(t, root, GrepToolName, GrepParams{Pattern: "hello", TotalMaxMatches: 1})
	if err != nil {
		t.Fatalf("grep truncated: %v", err)
	}
	if !strings.Contains(got, "results limited to 1 matches") {
		t.Errorf("grep truncated = %q", got)
	}

	// fixed_strings treats regex metacharacters literally.
	writeTestFile(t, root, "d.txt", "a.b\naxb\n")
	got, err = runTool(t, root, GrepToolName, GrepParams{Pattern: "a.b", FixedStrings: true, IncludePattern: "d.txt"})
	if err != nil {
		t.Fatalf("grep fixed_strings: %v", err)
	}
	if !strings.Contains(got, "Found 1 match ") {
		t.Errorf("grep fixed_strings = %q", got)
	}

	// Invalid regex is a tool error.
	if _, err := runTool(t, root, GrepToolName, GrepParams{Pattern: "("}); err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestReadManyFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "docs/a.md", "alpha")
	writeTestFile(t, root, "docs/b.md", "beta")
	writeTestFile(t, root, "docs/skip.bin", "bin\x00ary")
	writeTestFile(t, root, "node_modules/dep.md", "dependency")

	got, err := runTool(t, root, ReadManyFilesToolName, ReadManyFilesParams{Include: []string{"docs/**"}})
	if err != nil {
		t.Fatalf("readManyFiles: %v", err)
	}
	for _, want := range []string{
		fmt.Sprintf("--- %s ---", filepath.Join(root, "docs/a.md")),
		"alpha",
		"beta",
		"--- End of content ---",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("readManyFiles missing %q in %q", want, got)
		}
	}
	if !strings.Contains(got, "skip.bin (binary file)") {
		t.Errorf("readManyFiles should report skipped binary: %q", got)
	}
	if strings.Contains(got, "dependency") {
		t.Errorf("readManyFiles should not read node_modules: %q", got)
	}

	// A directory include reads everything under it.
	got, err = runTool(t, root, ReadManyFilesToolName, ReadManyFilesParams{Include: []string{"docs"}})
	if err != nil {
		t.Fatalf("readManyFiles dir include: %v", err)
	}
	if !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Errorf("readManyFiles dir include = %q", got)
	}

	// No matches.
	got, err = runTool(t, root, ReadManyFilesToolName, ReadManyFilesParams{Include: []string{"nothing/**"}})
	if err != nil {
		t.Fatalf("readManyFiles no match: %v", err)
	}
	if got != "No files matching the criteria were found." {
		t.Errorf("readManyFiles no match = %q", got)
	}

	// Include is required.
	if _, err := runTool(t, root, ReadManyFilesToolName, ReadManyFilesParams{}); err == nil {
		t.Error("expected error for missing include")
	}
}
