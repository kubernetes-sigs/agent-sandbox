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

	"github.com/stretchr/testify/require"
)

func TestSanitizePathInsideRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.py"), []byte("print()"), 0o644))

	got, err := SanitizePath(root, "main.py")
	require.NoError(t, err)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedRoot, "main.py"), got)
}

func TestSanitizePathRootItself(t *testing.T) {
	root := t.TempDir()

	got, err := SanitizePath(root, ".")
	require.NoError(t, err)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, resolvedRoot, got)
}

func TestSanitizePathTraversalRejected(t *testing.T) {
	root := t.TempDir()

	for _, p := range []string{
		"../etc/passwd",
		"../../etc/passwd",
		"a/../../etc/passwd",
		"..",
	} {
		_, err := SanitizePath(root, p)
		require.ErrorIs(t, err, ErrPathEscapes, "path %q should be rejected", p)
	}
}

func TestSanitizePathAbsolutePathConfined(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))

	// Absolute user paths are treated as relative to the sandbox root.
	got, err := SanitizePath(root, "/f.txt")
	require.NoError(t, err)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedRoot, "f.txt"), got)
}

func TestSanitizePathSymlinkEscapeRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	_, err := SanitizePath(root, "link/secret")
	require.ErrorIs(t, err, ErrPathEscapes)

	// Writing a new file through an escaping symlinked parent must also fail.
	_, err = SanitizePath(root, "link/newfile.txt")
	require.ErrorIs(t, err, ErrPathEscapes)
}

func TestSanitizePathPrefixSiblingRejected(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	evil := filepath.Join(base, "workspace-evil")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(evil, 0o755))

	_, err := SanitizePath(root, "../workspace-evil/f.txt")
	require.ErrorIs(t, err, ErrPathEscapes)
}

func TestSanitizePathNonExistentWriteTarget(t *testing.T) {
	root := t.TempDir()

	got, err := SanitizePath(root, "sub/dir/new.txt")
	require.NoError(t, err)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	// Only the closest existing parent is resolved; the remaining
	// components are preserved lexically.
	require.Equal(t, filepath.Join(resolvedRoot, "sub", "dir", "new.txt"), got)
}
