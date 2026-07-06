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

package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizePath(t *testing.T) {
	tempDir := t.TempDir()
	cleanTempDir, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		cleanTempDir = tempDir
	}

	subDir := filepath.Join(cleanTempDir, "allowed")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("failed to create subDir: %v", err)
	}

	tests := []struct {
		name      string
		rootDir   string
		userPath  string
		expectErr bool
	}{
		{
			name:      "Valid subpath",
			rootDir:   cleanTempDir,
			userPath:  "allowed/file.txt",
			expectErr: false,
		},
		{
			name:      "Root dir itself",
			rootDir:   cleanTempDir,
			userPath:  "",
			expectErr: false,
		},
		{
			name:      "Dot relative path",
			rootDir:   cleanTempDir,
			userPath:  "./allowed/file.txt",
			expectErr: false,
		},
		{
			name:      "Parent traversal attack",
			rootDir:   subDir,
			userPath:  "../secret.txt",
			expectErr: true,
		},
		{
			name:      "Prefix collision attack (e.g. /workspace-evil vs /workspace)",
			rootDir:   subDir,
			userPath:  "../allowed-evil/secret.txt",
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SanitizePath(tt.rootDir, tt.userPath)
			if (err != nil) != tt.expectErr {
				t.Errorf("SanitizePath() error = %v, expectErr %v", err, tt.expectErr)
				return
			}
			if !tt.expectErr && got == "" {
				t.Errorf("SanitizePath() returned empty string for valid input")
			}
		})
	}
}
