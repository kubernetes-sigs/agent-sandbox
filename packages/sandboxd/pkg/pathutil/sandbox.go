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

// Package pathutil confines user-supplied paths to the sandbox root
// directory, protecting against path traversal and symlink escapes as
// required by KEP-539.2.
package pathutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathEscapes is returned when a user-supplied path resolves outside the
// sandbox root. Callers map it to protocol-specific errors (HTTP 403,
// gRPC PERMISSION_DENIED).
var ErrPathEscapes = errors.New("path escapes sandbox root")

// SanitizePath takes a sandbox root directory and a user-provided relative or
// absolute path, cleans it, evaluates symlinks, and enforces that the
// resolved target path lies within the sandbox root directory.
//
// For paths that do not exist yet (e.g. the target of a write), symlinks are
// evaluated on the closest existing parent directory instead, so new files
// cannot be smuggled outside the root through a symlinked parent.
func SanitizePath(rootDir, userPath string) (string, error) {
	if rootDir == "" {
		rootDir = "/"
	}
	cleanRoot, err := filepath.Abs(filepath.Clean(rootDir))
	if err != nil {
		return "", fmt.Errorf("invalid sandbox root dir: %w", err)
	}

	cleanRootSymlink, err := filepath.EvalSymlinks(cleanRoot)
	if err == nil {
		cleanRoot = cleanRootSymlink
	}

	// Join root with user path directly to preserve relative ".." components
	// so they are neutralized by Clean before symlink evaluation.
	joined := filepath.Clean(filepath.Join(cleanRoot, userPath))

	// Resolve symlinks. If the joined path doesn't exist yet (e.g. Write or
	// MkdirAll targets), resolve symlinks on the existing parent directory
	// instead.
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if os.IsNotExist(err) {
			parent := filepath.Dir(joined)
			resolvedParent, parentErr := filepath.EvalSymlinks(parent)
			if parentErr == nil {
				resolved = filepath.Join(resolvedParent, filepath.Base(joined))
			} else {
				resolved = joined
			}
		} else {
			return "", fmt.Errorf("failed to evaluate symlinks: %w", err)
		}
	}

	// Ensure the resolved path matches cleanRoot exactly or starts with
	// cleanRoot + separator. This prevents prefix matching bugs
	// (e.g. /workspace-evil vs /workspace).
	if resolved != cleanRoot && !strings.HasPrefix(resolved, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("access denied: path %q: %w", userPath, ErrPathEscapes)
	}

	return resolved, nil
}
