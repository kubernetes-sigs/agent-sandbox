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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadConfigDir_NonExistentDir(t *testing.T) {
	data, err := readConfigDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got %v", err)
	}
	if data != nil {
		t.Errorf("expected nil data for missing dir, got %v", data)
	}
}

func TestReadConfigDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	data, err := readConfigDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty data, got %v", data)
	}
}

func TestReadConfigDir_ReadsFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key-a"), []byte("value-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key-b"), []byte("value-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := readConfigDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(data))
	}
	if data["key-a"] != "value-a" {
		t.Errorf("key-a = %q, want %q", data["key-a"], "value-a")
	}
	if data["key-b"] != "value-b" {
		t.Errorf("key-b = %q, want %q", data["key-b"], "value-b")
	}
}

func TestReadConfigDir_SkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := readConfigDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 key (subdir skipped), got %d: %v", len(data), data)
	}
}

func TestReadConfigDir_SkipsDotDotPrefixed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "..data"), []byte("metadata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := readConfigDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 key (..data skipped), got %d: %v", len(data), data)
	}
	if _, ok := data["..data"]; ok {
		t.Error("..data should have been skipped")
	}
}

func TestReadConfigDir_FollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-file")
	if err := os.WriteFile(target, []byte("symlinked-value"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	data, err := readConfigDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["link"] != "symlinked-value" {
		t.Errorf("link = %q, want %q", data["link"], "symlinked-value")
	}
}
