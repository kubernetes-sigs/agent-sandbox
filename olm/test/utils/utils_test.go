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

package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetProjectDir(t *testing.T) {
	t.Chdir(filepath.Join("..", "e2e"))

	dir, err := GetProjectDir()
	if err != nil {
		t.Fatalf("GetProjectDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "PROJECT")); err != nil {
		t.Fatalf("expected PROJECT in %q: %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Makefile")); err != nil {
		t.Fatalf("expected Makefile in %q: %v", dir, err)
	}
}
