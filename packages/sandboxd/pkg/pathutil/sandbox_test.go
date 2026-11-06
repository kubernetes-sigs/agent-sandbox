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
	"strings"
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

	// Full absolute path starting with root is also confined cleanly.
	gotFull, err := SanitizePath(root, filepath.Join(root, "f.txt"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedRoot, "f.txt"), gotFull)
}

func TestSanitizePathAbsolutePathConfinedThroughRootSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	rootAlias := filepath.Join(base, "root-alias")
	require.NoError(t, os.Mkdir(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(root, rootAlias))

	got, err := SanitizePath(rootAlias, filepath.Join(rootAlias, "f.txt"))
	require.NoError(t, err)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedRoot, "f.txt"), got)
}

func TestSanitizePathSiblingPrefixAbsolute(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.MkdirAll(root, 0o755))
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	// An absolute path sharing a prefix with root (/workspace-evil/f.txt)
	// must be treated as relative under root without string-mangling the prefix.
	got, err := SanitizePath(root, "/workspace-evil/f.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedRoot, "workspace-evil", "f.txt"), got)
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

	// Multi-level write through escaping symlink must also fail even when intermediate subdirs don't exist.
	_, err = SanitizePath(root, "link/subdir/newfile.txt")
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

func TestSanitizePathFilesystemRoot(t *testing.T) {
	dir := t.TempDir()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(resolvedDir, "f.txt"), []byte("x"), 0o644))

	// With "/" as the root, an absolute path is itself.
	got, err := SanitizePath("/", filepath.Join(resolvedDir, "f.txt"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedDir, "f.txt"), got)

	// So is a write target that does not exist yet.
	got, err = SanitizePath("/", filepath.Join(resolvedDir, "new", "g.txt"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedDir, "new", "g.txt"), got)

	// A relative path resolves against "/", and ".." cannot climb above it.
	got, err = SanitizePath("/", strings.TrimPrefix(filepath.Join(resolvedDir, "f.txt"), "/"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedDir, "f.txt"), got)

	got, err = SanitizePath("/", "../..")
	require.NoError(t, err)
	require.Equal(t, "/", got)
}

func TestSanitizeEntryPathKeepsFinalSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dir"), 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "dir", "escape")))
	require.NoError(t, os.Symlink("real", filepath.Join(root, "dir", "inside")))
	require.NoError(t, os.Symlink("dir", filepath.Join(root, "alias")))
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	// A final symlink is returned as is, even when it points outside the root.
	for _, p := range []string{"dir/escape", "dir/inside", "/dir/escape", filepath.Join(root, "dir", "escape")} {
		got, err := SanitizeEntryPath(root, p)
		require.NoError(t, err, p)
		require.Equal(t, filepath.Join(resolvedRoot, "dir", filepath.Base(p)), got, p)
	}

	// Symlinks in parent components are still resolved.
	got, err := SanitizeEntryPath(root, "alias/escape")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedRoot, "dir", "escape"), got)

	// So the entry beneath an escaping parent is rejected.
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "out")))
	_, err = SanitizeEntryPath(root, "out/secret")
	require.ErrorIs(t, err, ErrPathEscapes)
	_, err = SanitizeEntryPath(root, "out/sub/new")
	require.ErrorIs(t, err, ErrPathEscapes)
}

func TestSanitizeEntryPathRootAndTraversal(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(base, "workspace-evil"), 0o755))
	require.NoError(t, os.MkdirAll(root, 0o755))
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	for _, p := range []string{"", ".", "/", "a/..", root} {
		got, err := SanitizeEntryPath(root, p)
		require.NoError(t, err, p)
		require.Equal(t, resolvedRoot, got, p)
	}
	for _, p := range []string{"..", "../x", "a/../../x", "../workspace-evil/f.txt"} {
		_, err := SanitizeEntryPath(root, p)
		require.ErrorIs(t, err, ErrPathEscapes, p)
	}

	// A path that does not exist yet resolves like SanitizePath does.
	got, err := SanitizeEntryPath(root, "sub/dir/new.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedRoot, "sub", "dir", "new.txt"), got)
}
