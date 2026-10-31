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

package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func newRESTFixture(t *testing.T) (*RESTServer, http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	s := NewRESTServer(root, "SANDBOX_", logr.Discard())
	return s, s.Handler(), root
}

func doRequest(handler http.Handler, method, target string, body []byte, header http.Header) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for k, vals := range header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var apiErr APIError
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
	require.NotEmpty(t, apiErr.Code)
	require.NotEmpty(t, apiErr.Message)
	return apiErr
}

func TestPutThenGetFileRoundTrip(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	rec := doRequest(handler, http.MethodPut, "/v1/files/hello.txt", []byte("payload"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	onDisk, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	require.NoError(t, err)
	require.Equal(t, "payload", string(onDisk))

	rec = doRequest(handler, http.MethodGet, "/v1/files/hello.txt", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
	require.Equal(t, "payload", rec.Body.String())
}

func TestPutCreatesParentDirectories(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	rec := doRequest(handler, http.MethodPut, "/v1/files/a/b/c/deep.txt", []byte("x"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, err := os.Stat(filepath.Join(root, "a", "b", "c", "deep.txt"))
	require.NoError(t, err)
}

func TestPutAppliesMode(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	rec := doRequest(handler, http.MethodPut, "/v1/files/run.sh?mode=0755", []byte("#!/bin/sh"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	info, err := os.Stat(filepath.Join(root, "run.sh"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestPutInvalidModeRejected(t *testing.T) {
	_, handler, _ := newRESTFixture(t)

	for _, mode := range []string{"0999", "777", "abcd", "01777"} {
		rec := doRequest(handler, http.MethodPut, "/v1/files/f.txt?mode="+mode, []byte("x"), nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, "mode %q must be rejected", mode)
		require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)
	}
}

func TestPutOverwritesAtomically(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	require.Equal(t, http.StatusNoContent,
		doRequest(handler, http.MethodPut, "/v1/files/f.txt", []byte("old"), nil).Code)
	require.Equal(t, http.StatusNoContent,
		doRequest(handler, http.MethodPut, "/v1/files/f.txt", []byte("new"), nil).Code)

	onDisk, err := os.ReadFile(filepath.Join(root, "f.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(onDisk))

	// No leftover temp files from the atomic write strategy.
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestPutAppendAddsToFile(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	// The first append creates the file, including its parent directories.
	for _, chunk := range []string{"one,", "two"} {
		rec := doRequest(handler, http.MethodPut, "/v1/files/a/log.txt?append=true", []byte(chunk), nil)
		require.Equal(t, http.StatusNoContent, rec.Code)
	}

	onDisk, err := os.ReadFile(filepath.Join(root, "a", "log.txt"))
	require.NoError(t, err)
	require.Equal(t, "one,two", string(onDisk))
}

func TestPutAppendAppliesModeOnlyToNewFile(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	for _, mode := range []string{"0755", "0600"} {
		rec := doRequest(handler, http.MethodPut, "/v1/files/run.sh?append=true&mode="+mode, []byte("x"), nil)
		require.Equal(t, http.StatusNoContent, rec.Code)
	}

	info, err := os.Stat(filepath.Join(root, "run.sh"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestPutAppendFalseOverwrites(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	require.Equal(t, http.StatusNoContent,
		doRequest(handler, http.MethodPut, "/v1/files/f.txt", []byte("old"), nil).Code)
	require.Equal(t, http.StatusNoContent,
		doRequest(handler, http.MethodPut, "/v1/files/f.txt?append=false", []byte("new"), nil).Code)

	onDisk, err := os.ReadFile(filepath.Join(root, "f.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(onDisk))
}

func TestPutAppendRejections(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "d"), 0o755))

	for _, target := range []string{"/v1/files/f.txt?append=banana", "/v1/files/d?append=true"} {
		rec := doRequest(handler, http.MethodPut, target, []byte("x"), nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, target)
		require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)
	}
}

func TestPutDirectoryCreatesEmptyDirectory(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	// The body is ignored, and a retry succeeds without touching the mode.
	for _, target := range []string{"/v1/files/d?type=directory", "/v1/files/d?type=directory&mode=0700"} {
		rec := doRequest(handler, http.MethodPut, target, []byte("ignored"), nil)
		require.Equal(t, http.StatusNoContent, rec.Code, target)
	}

	info, err := os.Stat(filepath.Join(root, "d"))
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	entries, err := os.ReadDir(filepath.Join(root, "d"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestPutDirectoryAppliesMode(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	rec := doRequest(handler, http.MethodPut, "/v1/files/d?type=directory&mode=0700", nil, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	info, err := os.Stat(filepath.Join(root, "d"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestPutDirectoryParents(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	rec := doRequest(handler, http.MethodPut, "/v1/files/a/b?type=directory", nil, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "NOT_FOUND", decodeError(t, rec).Code)
	require.NoDirExists(t, filepath.Join(root, "a"))

	rec = doRequest(handler, http.MethodPut, "/v1/files/a/b?type=directory&parents=true&mode=0700", nil, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// Only the requested directory gets the mode; parents get the default.
	parent, err := os.Stat(filepath.Join(root, "a"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), parent.Mode().Perm())
	leaf, err := os.Stat(filepath.Join(root, "a", "b"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), leaf.Mode().Perm())
}

func TestPutDirectoryRejections(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	tests := []struct {
		name       string
		target     string
		wantStatus int
		wantCode   string
	}{
		{"path is a file", "f.txt?type=directory", http.StatusConflict, "CONFLICT"},
		{"append", "d?type=directory&append=true", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"invalid parents", "d?type=directory&parents=banana", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"invalid mode", "d?type=directory&mode=999", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"unknown type", "d?type=link", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"symlink escape", "link/d?type=directory", http.StatusForbidden, "PERMISSION_DENIED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(handler, http.MethodPut, "/v1/files/"+tc.target, nil, nil)
			require.Equal(t, tc.wantStatus, rec.Code)
			require.Equal(t, tc.wantCode, decodeError(t, rec).Code)
		})
	}
	require.NoDirExists(t, filepath.Join(outside, "d"))
}

func TestPutTypeFileWritesFile(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	rec := doRequest(handler, http.MethodPut, "/v1/files/f.txt?type=file", []byte("x"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	onDisk, err := os.ReadFile(filepath.Join(root, "f.txt"))
	require.NoError(t, err)
	require.Equal(t, "x", string(onDisk))
}

func TestPutTypeFileOnDirectoryConflicts(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.Mkdir(filepath.Join(root, "d"), 0o755))

	rec := doRequest(handler, http.MethodPut, "/v1/files/d?type=file", []byte("x"), nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "CONFLICT", decodeError(t, rec).Code)
}

func TestPutDirectoryPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.Mkdir(filepath.Join(root, "ro"), 0o500))

	rec := doRequest(handler, http.MethodPut, "/v1/files/ro/d?type=directory", nil, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "PERMISSION_DENIED", decodeError(t, rec).Code)
}

func TestPutMultipartUpload(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "upload.bin")
	require.NoError(t, err)
	_, err = part.Write([]byte("multipart-payload"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	header := http.Header{"Content-Type": []string{mw.FormDataContentType()}}
	rec := doRequest(handler, http.MethodPut, "/v1/files/upload.bin", buf.Bytes(), header)
	require.Equal(t, http.StatusNoContent, rec.Code)

	onDisk, err := os.ReadFile(filepath.Join(root, "upload.bin"))
	require.NoError(t, err)
	require.Equal(t, "multipart-payload", string(onDisk))
}

func TestPutMultipartMissingFilePart(t *testing.T) {
	_, handler, _ := newRESTFixture(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("notfile", "x"))
	require.NoError(t, mw.Close())

	header := http.Header{"Content-Type": []string{mw.FormDataContentType()}}
	rec := doRequest(handler, http.MethodPut, "/v1/files/f.txt", buf.Bytes(), header)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)
}

func TestPutToDirectoryRejected(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "d"), 0o755))

	rec := doRequest(handler, http.MethodPut, "/v1/files/d", []byte("x"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPutEncodedSlashesCreateNestedFile(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	// Clients may %2F-encode separators inside a single path segment; the
	// server decodes the full path before resolving it (spec).
	rec := doRequest(handler, http.MethodPut, "/v1/files/dir%2Fsub%2Ffile.txt", []byte("x"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, err := os.Stat(filepath.Join(root, "dir", "sub", "file.txt"))
	require.NoError(t, err)
}

func TestPutCreateOnly(t *testing.T) {
	for _, query := range []string{"", "?append=true"} {
		t.Run("query"+query, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			header := http.Header{"If-None-Match": []string{"*"}}

			rec := doRequest(handler, http.MethodPut, "/v1/files/a/f.txt"+query, []byte("first"), header)
			require.Equal(t, http.StatusNoContent, rec.Code)

			rec = doRequest(handler, http.MethodPut, "/v1/files/a/f.txt"+query, []byte("second"), header)
			require.Equal(t, http.StatusPreconditionFailed, rec.Code)
			require.Equal(t, "FAILED_PRECONDITION", decodeError(t, rec).Code)

			onDisk, err := os.ReadFile(filepath.Join(root, "a", "f.txt"))
			require.NoError(t, err)
			require.Equal(t, "first", string(onDisk))
			entries, err := os.ReadDir(filepath.Join(root, "a"))
			require.NoError(t, err)
			require.Len(t, entries, 1, "no temp file may be left behind")
		})
	}
}

func TestPutCreateOnlyExistingDirectory(t *testing.T) {
	for _, query := range []string{"", "?append=true"} {
		t.Run("query"+query, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			require.NoError(t, os.MkdirAll(filepath.Join(root, "d"), 0o755))
			header := http.Header{"If-None-Match": []string{"*"}}

			rec := doRequest(handler, http.MethodPut, "/v1/files/d"+query, []byte("x"), header)
			require.Equal(t, http.StatusPreconditionFailed, rec.Code)
			require.Equal(t, "FAILED_PRECONDITION", decodeError(t, rec).Code)
		})
	}
}

func TestPutCreateOnlyWithType(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "d"), 0o755))
	header := http.Header{"If-None-Match": []string{"*"}}

	rec := doRequest(handler, http.MethodPut, "/v1/files/d?type=directory", nil, header)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)

	// The precondition outranks the type mismatch.
	rec = doRequest(handler, http.MethodPut, "/v1/files/d?type=file", []byte("x"), header)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestPutCreateOnlyRacesToOneWinner(t *testing.T) {
	for _, query := range []string{"", "?append=true"} {
		t.Run("query"+query, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			header := http.Header{"If-None-Match": []string{"*"}}

			const writers = 16
			codes := make(chan int, writers)
			for range writers {
				go func() {
					codes <- doRequest(handler, http.MethodPut, "/v1/files/f.txt"+query, []byte("x"), header).Code
				}()
			}
			created := 0
			for range writers {
				if code := <-codes; code == http.StatusNoContent {
					created++
				} else {
					require.Equal(t, http.StatusPreconditionFailed, code)
				}
			}
			require.Equal(t, 1, created)

			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Len(t, entries, 1, "no temp file may be left behind")
		})
	}
}

func TestPutIfNoneMatchRejectsOtherValues(t *testing.T) {
	_, handler, _ := newRESTFixture(t)

	for _, value := range []string{`"etag"`, "W/*", ""} {
		header := http.Header{"If-None-Match": []string{value}}
		rec := doRequest(handler, http.MethodPut, "/v1/files/f.txt", []byte("x"), header)
		require.Equal(t, http.StatusBadRequest, rec.Code, "value %q must be rejected", value)
		require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)
	}
}

func TestPutPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	for _, query := range []string{"", "?append=true"} {
		t.Run("query"+query, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			require.NoError(t, os.Mkdir(filepath.Join(root, "ro"), 0o500))
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "ro"), 0o755) })

			rec := doRequest(handler, http.MethodPut, "/v1/files/ro/f.txt"+query, []byte("x"), nil)
			require.Equal(t, http.StatusForbidden, rec.Code)
			require.Equal(t, "PERMISSION_DENIED", decodeError(t, rec).Code)
		})
	}
}

func TestGetDirectoryListing(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("bb"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "adir"), 0o755))

	rec := doRequest(handler, http.MethodGet, "/v1/files/", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var listing DirectoryListing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listing))
	require.NotEmpty(t, listing.Path)
	require.Len(t, listing.Entries, 2)

	// os.ReadDir returns entries sorted by name.
	require.Equal(t, "adir", listing.Entries[0].Name)
	require.Equal(t, "directory", listing.Entries[0].Type)
	require.Equal(t, "b.txt", listing.Entries[1].Name)
	require.Equal(t, "file", listing.Entries[1].Type)
	require.Equal(t, int64(2), listing.Entries[1].Size)
	require.Equal(t, "0600", listing.Entries[1].Mode)

	_, err := time.Parse(time.RFC3339, listing.Entries[1].ModifiedAt)
	require.NoError(t, err, "modified_at must be RFC 3339")
}

func TestGetEmptyDirectoryListsEmptyEntries(t *testing.T) {
	_, handler, _ := newRESTFixture(t)

	rec := doRequest(handler, http.MethodGet, "/v1/files/", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"entries":[]`, "entries must be [] not null")
}

func listingNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code)
	var listing DirectoryListing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listing))
	names := make([]string, 0, len(listing.Entries))
	for _, e := range listing.Entries {
		names = append(names, e.Name)
	}
	return names
}

func TestGetRecursiveListing(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "c.txt"), []byte("c"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "x.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "z.txt"), []byte("z"), 0o644))

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"flat by default", "", []string{"a", "b.txt", "z.txt"}},
		{"unlimited depth", "recursive=true", []string{"a", "a/b", "a/b/c.txt", "a/x.txt", "b.txt", "z.txt"}},
		{"depth 1 is flat", "recursive=true&max_depth=1", []string{"a", "b.txt", "z.txt"}},
		{"depth 2", "recursive=true&max_depth=2", []string{"a", "a/b", "a/x.txt", "b.txt", "z.txt"}},
		{"depth beyond tree", "recursive=true&max_depth=9", []string{"a", "a/b", "a/b/c.txt", "a/x.txt", "b.txt", "z.txt"}},
		{"max_depth needs recursive", "max_depth=3", []string{"a", "b.txt", "z.txt"}},
		{"recursive=false is flat", "recursive=false", []string{"a", "b.txt", "z.txt"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(handler, http.MethodGet, "/v1/files/?"+tc.query, nil, nil)
			require.Equal(t, tc.want, listingNames(t, rec))
		})
	}
}

func TestGetRecursiveListingEntryCap(t *testing.T) {
	s, handler, root := newRESTFixture(t)
	s.maxListEntries = 5
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "c.txt"), []byte("c"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "x.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("b"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "z.txt"), []byte("z"), 0o644))

	// The full tree has 6 entries: one over the cap.
	rec := doRequest(handler, http.MethodGet, "/v1/files/?recursive=true", nil, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)

	// Exactly at the cap is allowed, and so are listings that are not recursive.
	rec = doRequest(handler, http.MethodGet, "/v1/files/?recursive=true&max_depth=2", nil, nil)
	require.Len(t, listingNames(t, rec), 5)
	rec = doRequest(handler, http.MethodGet, "/v1/files/", nil, nil)
	require.Len(t, listingNames(t, rec), 3)
}

func TestGetRecursiveListingIsRelativeToRequestedDirectory(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "c.txt"), []byte("c"), 0o644))

	rec := doRequest(handler, http.MethodGet, "/v1/files/a?recursive=true", nil, nil)
	require.Equal(t, []string{"b", "b/c.txt"}, listingNames(t, rec))

	var listing DirectoryListing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listing))
	require.Equal(t, "/a", listing.Path)
	require.Equal(t, "directory", listing.Entries[0].Type)
	require.Equal(t, "file", listing.Entries[1].Type)
	require.Equal(t, int64(1), listing.Entries[1].Size)
}

func TestGetRecursiveListingInvalidQuery(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))

	for _, query := range []string{
		"recursive=banana",
		"recursive=true&max_depth=0",
		"recursive=true&max_depth=-1",
		"recursive=true&max_depth=deep",
		"max_depth=0",
	} {
		// The query is rejected for files and missing paths too, before lookup.
		for _, path := range []string{"", "f.txt", "nope"} {
			rec := doRequest(handler, http.MethodGet, "/v1/files/"+path+"?"+query, nil, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s?%s", path, query)
			require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)
		}
	}
}

func TestGetRecursiveListingSkipsSymlinkTargets(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "f.txt"), []byte("f"), 0o644))
	// A link inside the root is listed but not entered; one that leaves the
	// root is omitted even when nested.
	require.NoError(t, os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "a", "escape")))

	rec := doRequest(handler, http.MethodGet, "/v1/files/?recursive=true", nil, nil)
	require.Equal(t, []string{"a", "a/f.txt", "link"}, listingNames(t, rec))
}

func TestGetRecursiveListingKeepsUnreadableSubdirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	_, handler, root := newRESTFixture(t)
	locked := filepath.Join(root, "locked")
	require.NoError(t, os.MkdirAll(filepath.Join(locked, "inner"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "z.txt"), []byte("z"), 0o644))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	rec := doRequest(handler, http.MethodGet, "/v1/files/?recursive=true", nil, nil)
	require.Equal(t, []string{"locked", "z.txt"}, listingNames(t, rec))
}

func TestGetRecursiveListingStopsWhenRequestCanceled(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/files/?recursive=true", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "INTERNAL", decodeError(t, rec).Code)
}

func TestGetMissingFileNotFound(t *testing.T) {
	_, handler, _ := newRESTFixture(t)

	rec := doRequest(handler, http.MethodGet, "/v1/files/nope.txt", nil, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "NOT_FOUND", decodeError(t, rec).Code)
}

func TestHeadFileExistenceProbe(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("12345"), 0o644))

	rec := doRequest(handler, http.MethodHead, "/v1/files/f.txt", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Body.String())

	rec = doRequest(handler, http.MethodHead, "/v1/files/absent.txt", nil, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetTypeExpectation(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "d"), 0o755))

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantCode   string
	}{
		{"file expected, file found", "f.txt?type=file", http.StatusOK, ""},
		{"directory expected, directory found", "d?type=directory", http.StatusOK, ""},
		{"no expectation, file", "f.txt", http.StatusOK, ""},
		{"no expectation, directory", "d", http.StatusOK, ""},
		{"file expected, directory found", "d?type=file", http.StatusConflict, "CONFLICT"},
		{"directory expected, file found", "f.txt?type=directory", http.StatusConflict, "CONFLICT"},
		{"missing path stays not found", "nope?type=file", http.StatusNotFound, "NOT_FOUND"},
		{"unknown type is invalid", "f.txt?type=link", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"empty type is invalid", "f.txt?type=", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"unknown type is invalid before lookup", "nope?type=link", http.StatusBadRequest, "INVALID_ARGUMENT"},
	}
	for _, tc := range tests {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				rec := doRequest(handler, method, "/v1/files/"+tc.path, nil, nil)
				require.Equal(t, tc.wantStatus, rec.Code)
				if tc.wantCode != "" && method == http.MethodGet {
					require.Equal(t, tc.wantCode, decodeError(t, rec).Code)
				}
			})
		}
	}
}

func TestTraversalRejected(t *testing.T) {
	_, handler, _ := newRESTFixture(t)

	// %2E%2E dodges the mux's lexical path cleaning, so this exercises the
	// SanitizePath defense in depth.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := doRequest(handler, method, "/v1/files/%2E%2E/%2E%2E/etc/passwd", []byte("x"), nil)
		require.Equal(t, http.StatusForbidden, rec.Code, "method %s must reject traversal", method)
		require.Equal(t, "PERMISSION_DENIED", decodeError(t, rec).Code)
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	rec := doRequest(handler, http.MethodGet, "/v1/files/link/secret", nil, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestDeleteFile(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))

	rec := doRequest(handler, http.MethodDelete, "/v1/files/f.txt", nil, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	rec = doRequest(handler, http.MethodDelete, "/v1/files/f.txt", nil, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteNonEmptyDirectoryConflict(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "d", "f.txt"), []byte("x"), 0o644))

	rec := doRequest(handler, http.MethodDelete, "/v1/files/d", nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "CONFLICT", decodeError(t, rec).Code)

	rec = doRequest(handler, http.MethodDelete, "/v1/files/d?recursive=true", nil, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	_, err := os.Stat(filepath.Join(root, "d"))
	require.True(t, os.IsNotExist(err))
}

func TestDeleteRootDirectoryEmptiesContents(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))

	// Non-recursive DELETE /v1/files/ must fail with 409 CONFLICT.
	rec := doRequest(handler, http.MethodDelete, "/v1/files/", nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code)

	// Recursive DELETE /v1/files/?recursive=true clears contents but leaves root intact.
	rec = doRequest(handler, http.MethodDelete, "/v1/files/?recursive=true", nil, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestDirectoryListingOmitsEscapingSymlinks(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escaping_link")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "normal.txt"), []byte("normal"), 0o644))

	rec := doRequest(handler, http.MethodGet, "/v1/files/", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var listing DirectoryListing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listing))
	require.Len(t, listing.Entries, 1)
	require.Equal(t, "normal.txt", listing.Entries[0].Name)
}

func TestDeleteInvalidRecursiveRejected(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))

	rec := doRequest(handler, http.MethodDelete, "/v1/files/f.txt?recursive=banana", nil, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHealthReadyAndShutdown(t *testing.T) {
	s, handler, _ := newRESTFixture(t)

	rec := doRequest(handler, http.MethodGet, "/v1/health", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var health HealthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &health))
	require.Equal(t, "ok", health.Status)
	require.GreaterOrEqual(t, health.UptimeSeconds, int64(0))

	s.SetReady(false)
	rec = doRequest(handler, http.MethodGet, "/v1/health", nil, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "UNAVAILABLE", decodeError(t, rec).Code)
}

func TestMetadataFiltersEnv(t *testing.T) {
	s, _, _ := newRESTFixture(t)
	s.environ = func() []string {
		return []string{
			"SANDBOX_ID=sbx-123",
			"SANDBOX_WORKSPACE=/workspace",
			"SANDBOX_API_TOKEN=super-secret",   // sensitive marker: filtered
			"SANDBOX_SSH_KEY=super-secret",     // sensitive marker: filtered
			"SANDBOX_AUTH_HEADER=super-secret", // sensitive marker: filtered
			"SANDBOX_BEARER=super-secret",      // sensitive marker: filtered
			"SANDBOX_CRED=super-secret",        // sensitive marker: filtered
			"SANDBOX_PASSWD=super-secret",      // sensitive marker: filtered
			"SANDBOX_TLS_CERT=super-secret",    // sensitive marker: filtered
			"SANDBOX_PRIVATE_URL=super-secret", // sensitive marker: filtered
			"KUBERNETES_SERVICE_HOST=1.2.3.4",  // wrong prefix: filtered
			"MALFORMED_NO_EQUALS",
		}
	}
	handler := s.Handler()

	rec := doRequest(handler, http.MethodGet, "/v1/metadata", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var meta MetadataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))
	require.Equal(t, map[string]string{
		"SANDBOX_ID":        "sbx-123",
		"SANDBOX_WORKSPACE": "/workspace",
	}, meta.Env)
}

func TestPutAbsolutePathWithFilesystemRoot(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	handler := NewRESTServer("/", "SANDBOX_", logr.Discard()).Handler()
	target := filepath.Join(dir, "solution", "solve.sh")

	rec := doRequest(handler, http.MethodPut, "/v1/files"+target+"?mode=0755", []byte("#!/bin/sh"), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	rec = doRequest(handler, http.MethodGet, "/v1/files"+target, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "#!/bin/sh", rec.Body.String())
}

func TestDeleteFilesystemRootRefused(t *testing.T) {
	handler := NewRESTServer("/", "SANDBOX_", logr.Discard()).Handler()
	// Probe without recursive first: if the guard regresses this gets 409 and
	// deletes nothing, so the recursive request below is never sent.
	for _, path := range []string{"/v1/files/", "/v1/files/?recursive=true"} {
		rec := doRequest(handler, http.MethodDelete, path, nil, nil)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
		require.Equal(t, "PERMISSION_DENIED", decodeError(t, rec).Code)
	}
}

// doAction posts a :move or :copy request for src.
func doAction(handler http.Handler, action, src string, query url.Values) *httptest.ResponseRecorder {
	return doRequest(handler, http.MethodPost, "/v1/files/"+src+":"+action+"?"+query.Encode(), nil, nil)
}

// writeFiles creates the named files under root. A name ending in "/" creates
// an empty directory.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if strings.HasSuffix(name, "/") {
			require.NoError(t, os.MkdirAll(path, 0o755))
			continue
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

// readTree maps every path below dir, relative to it, to its content. A
// directory maps to "" and carries a trailing "/", and a symlink or other
// special file maps to a description of itself.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel, err := filepath.Rel(dir, path)
		require.NoError(t, err)
		switch {
		case rel == ".":
		case d.IsDir():
			tree[rel+"/"] = ""
		case d.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			require.NoError(t, err)
			tree[rel] = "-> " + target
		case !d.Type().IsRegular():
			tree[rel] = "(" + d.Type().String() + ")"
		default:
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			tree[rel] = string(data)
		}
		return nil
	}))
	return tree
}

func TestMoveAndCopyFile(t *testing.T) {
	for _, action := range []string{actionMove, actionCopy} {
		t.Run(action, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			src := filepath.Join(root, "a.txt")
			writeFiles(t, root, map[string]string{"a.txt": "data"})
			require.NoError(t, os.Chmod(src, 0o755))

			rec := doAction(handler, action, "a.txt", url.Values{"destination": {"x/y/b.txt"}})
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

			dst := filepath.Join(root, "x", "y", "b.txt")
			onDisk, err := os.ReadFile(dst)
			require.NoError(t, err)
			require.Equal(t, "data", string(onDisk))
			info, err := os.Stat(dst)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

			_, err = os.Stat(src)
			if action == actionMove {
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMoveAndCopyDirectory(t *testing.T) {
	for _, action := range []string{actionMove, actionCopy} {
		t.Run(action, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			writeFiles(t, root, map[string]string{"d/f.txt": "f", "d/sub/g.txt": "g", "d/empty/": ""})
			want := readTree(t, filepath.Join(root, "d"))

			rec := doAction(handler, action, "d", url.Values{"destination": {"x/e"}})
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

			require.Equal(t, want, readTree(t, filepath.Join(root, "x", "e")))
			_, err := os.Stat(filepath.Join(root, "d"))
			if action == actionMove {
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCopyDirectoryKeepsModesAndSymlinks(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("s"), 0o644))
	writeFiles(t, root, map[string]string{"d/f.txt": "f"})
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "d", "link")))
	// A read-only directory must still receive its children before it is
	// made read-only itself.
	src, dst := filepath.Join(root, "d"), filepath.Join(root, "copy")
	require.NoError(t, os.Chmod(src, 0o555))
	t.Cleanup(func() {
		_ = os.Chmod(src, 0o755)
		_ = os.Chmod(dst, 0o755)
	})

	rec := doAction(handler, actionCopy, "d", url.Values{"destination": {"copy"}})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	info, err := os.Stat(dst)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o555), info.Mode().Perm())
	// The link is recreated, not followed, so nothing outside the root is read.
	target, err := os.Readlink(filepath.Join(dst, "link"))
	require.NoError(t, err)
	require.Equal(t, outside, target)
}

func TestMoveAndCopyDestinationConflicts(t *testing.T) {
	tests := []struct {
		name      string
		src, dst  string
		overwrite string
		wantCode  int
		wantData  string // content of dst afterwards, when it stays a file
	}{
		{name: "file exists", src: "a.txt", dst: "b.txt", wantCode: http.StatusConflict, wantData: "old"},
		{name: "overwrite replaces file", src: "a.txt", dst: "b.txt", overwrite: "true", wantCode: http.StatusNoContent, wantData: "new"},
		{name: "overwrite onto directory", src: "a.txt", dst: "dir", overwrite: "true", wantCode: http.StatusConflict},
		{name: "overwrite directory onto file", src: "dir", dst: "b.txt", overwrite: "true", wantCode: http.StatusConflict, wantData: "old"},
		{name: "directory onto directory", src: "dir", dst: "dir2", overwrite: "true", wantCode: http.StatusConflict},
	}
	for _, action := range []string{actionMove, actionCopy} {
		for _, tt := range tests {
			t.Run(action+"/"+tt.name, func(t *testing.T) {
				_, handler, root := newRESTFixture(t)
				writeFiles(t, root, map[string]string{"a.txt": "new", "b.txt": "old", "dir/f": "f", "dir2/": ""})

				query := url.Values{"destination": {tt.dst}}
				if tt.overwrite != "" {
					query.Set("overwrite", tt.overwrite)
				}
				rec := doAction(handler, action, tt.src, query)
				require.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
				if tt.wantCode == http.StatusConflict {
					require.Equal(t, "CONFLICT", decodeError(t, rec).Code)
				}
				if tt.wantData != "" {
					data, err := os.ReadFile(filepath.Join(root, "b.txt"))
					require.NoError(t, err)
					require.Equal(t, tt.wantData, string(data))
				}
			})
		}
	}
}

func TestMoveAndCopyRejections(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		query    url.Values
		wantCode int
		wantErr  string
	}{
		{"missing destination", "a.txt", url.Values{}, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"invalid overwrite", "a.txt", url.Values{"destination": {"b"}, "overwrite": {"maybe"}}, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"missing source", "nope.txt", url.Values{"destination": {"b"}}, http.StatusNotFound, "NOT_FOUND"},
		{"same path", "a.txt", url.Values{"destination": {"a.txt"}}, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"directory into itself", "d", url.Values{"destination": {"d/sub/d"}}, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"sandbox root", "", url.Values{"destination": {"d/root"}}, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"source traversal", "%2E%2E/%2E%2E/etc/passwd", url.Values{"destination": {"b"}}, http.StatusForbidden, "PERMISSION_DENIED"},
		{"destination traversal", "a.txt", url.Values{"destination": {"../../etc/passwd"}}, http.StatusForbidden, "PERMISSION_DENIED"},
		{"source through escaping symlink", "link/secret", url.Values{"destination": {"b"}}, http.StatusForbidden, "PERMISSION_DENIED"},
		{"destination through escaping symlink", "a.txt", url.Values{"destination": {"link/b"}}, http.StatusForbidden, "PERMISSION_DENIED"},
	}
	for _, action := range []string{actionMove, actionCopy} {
		for _, tt := range tests {
			t.Run(action+"/"+tt.name, func(t *testing.T) {
				_, handler, root := newRESTFixture(t)
				writeFiles(t, root, map[string]string{"a.txt": "a", "d/sub/": ""})
				outside := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644))
				require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

				rec := doAction(handler, action, tt.src, tt.query)
				require.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
				require.Equal(t, tt.wantErr, decodeError(t, rec).Code)
			})
		}
	}
}

func TestFileActionUnknown(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	writeFiles(t, root, map[string]string{"a.txt": "a"})

	for _, target := range []string{"/v1/files/a.txt", "/v1/files/a.txt:rename?destination=b", "/v1/files/a.txt:?destination=b"} {
		rec := doRequest(handler, http.MethodPost, target, nil, nil)
		require.Equal(t, http.StatusNotFound, rec.Code, target)
		require.Equal(t, "NOT_FOUND", decodeError(t, rec).Code)
	}
	require.Equal(t, map[string]string{"a.txt": "a"}, readTree(t, root))
}

func TestCopyUnsupportedFileType(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	writeFiles(t, root, map[string]string{"d/f.txt": "f"})
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "d", "pipe"), 0o644))

	// The copy fails part way through the tree and must not leave a partial
	// destination or temporary directory behind.
	rec := doAction(handler, actionCopy, "d", url.Values{"destination": {"x"}})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "d", entries[0].Name())

	// A move only renames, so it does not care about file types.
	rec = doAction(handler, actionMove, "d/pipe", url.Values{"destination": {"moved"}})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestMoveAndCopyActOnSymlinkItself(t *testing.T) {
	for _, action := range []string{actionMove, actionCopy} {
		t.Run(action, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			outside := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(outside, []byte("s"), 0o644))
			writeFiles(t, root, map[string]string{"real.txt": "data", "dir/f": "f"})
			// A relative link, one to a directory, and one that escapes the root.
			require.NoError(t, os.Symlink("real.txt", filepath.Join(root, "link")))
			require.NoError(t, os.Symlink("dir", filepath.Join(root, "dirlink")))
			require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))

			for src, want := range map[string]string{"link": "real.txt", "dirlink": "dir", "escape": outside} {
				rec := doAction(handler, action, src, url.Values{"destination": {"moved-" + src}})
				require.Equal(t, http.StatusNoContent, rec.Code, src+": "+rec.Body.String())

				target, err := os.Readlink(filepath.Join(root, "moved-"+src))
				require.NoError(t, err, src)
				require.Equal(t, want, target, src)
				_, err = os.Lstat(filepath.Join(root, src))
				if action == actionMove {
					require.ErrorIs(t, err, os.ErrNotExist, src)
				} else {
					require.NoError(t, err, src)
				}
			}
			// The targets are untouched.
			require.Equal(t, "data", readFile(t, filepath.Join(root, "real.txt")))
			require.Equal(t, "f", readFile(t, filepath.Join(root, "dir", "f")))
			require.Equal(t, "s", readFile(t, outside))
		})
	}
}

func TestMoveAndCopyOverwriteReplacesSymlinkItself(t *testing.T) {
	for _, action := range []string{actionMove, actionCopy} {
		for _, srcIsLink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/srcIsLink=%t", action, srcIsLink), func(t *testing.T) {
				_, handler, root := newRESTFixture(t)
				writeFiles(t, root, map[string]string{"a.txt": "new", "keep.txt": "keep"})
				require.NoError(t, os.Symlink("keep.txt", filepath.Join(root, "dst")))
				src := "a.txt"
				if srcIsLink {
					require.NoError(t, os.Symlink("a.txt", filepath.Join(root, "srclink")))
					src = "srclink"
				}

				rec := doAction(handler, action, src, url.Values{"destination": {"dst"}, "overwrite": {"true"}})
				require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

				// The link at dst is replaced, not written through.
				require.Equal(t, "keep", readFile(t, filepath.Join(root, "keep.txt")))
				info, err := os.Lstat(filepath.Join(root, "dst"))
				require.NoError(t, err)
				require.Equal(t, srcIsLink, info.Mode()&os.ModeSymlink != 0)
				require.Equal(t, "new", readFile(t, filepath.Join(root, "dst")))
			})
		}
	}
}

func TestMoveAndCopyBrokenSymlinkDestinationConflicts(t *testing.T) {
	for _, action := range []string{actionMove, actionCopy} {
		t.Run(action, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			writeFiles(t, root, map[string]string{"a.txt": "a"})
			require.NoError(t, os.Symlink("missing", filepath.Join(root, "dangling")))

			rec := doAction(handler, action, "a.txt", url.Values{"destination": {"dangling"}})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			_, err := os.Stat(filepath.Join(root, "missing"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestCopyLeafRefusesSwappedFile(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFiles(t, dir, map[string]string{"real": "data"})
	writeFiles(t, outside, map[string]string{"secret": "s"})
	// The stale info says "regular file", as if the path was swapped after the
	// caller's Lstat.
	stale, err := os.Lstat(filepath.Join(dir, "real"))
	require.NoError(t, err)
	relOutside, err := filepath.Rel(dir, filepath.Join(outside, "secret"))
	require.NoError(t, err)
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")))
	require.NoError(t, os.Symlink(relOutside, filepath.Join(dir, "rellink")))
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644))
	root := openTestRoot(t, dir)

	for _, name := range []string{"link", "rellink", "pipe"} {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(dir, "copy-of-"+name)
			require.Error(t, copyLeaf(root, name, dst, stale))
			_, err := os.Lstat(dst)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestCopyTreeStaysInsideRoot(t *testing.T) {
	base := t.TempDir()
	dir, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	writeFiles(t, base, map[string]string{"root/d/f": "f", "outside/secret": "s"})
	// A directory swapped for a symlink out of the root, as if after the
	// caller's Lstat, is not read through.
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "swapped")))
	root := openTestRoot(t, dir)
	dst := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, os.Mkdir(dst, 0o755))

	require.Error(t, copyTree(root, "swapped", dst, 0o755))
	require.Empty(t, readTree(t, dst))

	require.NoError(t, copyTree(root, "d", dst, 0o755))
	require.Equal(t, map[string]string{"f": "f"}, readTree(t, dst))
}

func TestMoveEntryFallsBackToCopyAcrossFilesystems(t *testing.T) {
	crossDevice := func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
	}
	for name, files := range map[string]map[string]string{
		"file":      {"src": "data"},
		"directory": {"src/f.txt": "f", "src/sub/g.txt": "g"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, files)
			src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
			want := map[string]string{}
			for rel, content := range readTree(t, dir) {
				want[strings.Replace(rel, "src", "dst", 1)] = content
			}

			require.NoError(t, moveEntry(crossDevice, openTestRoot(t, dir), src, dst))

			// Only dst is left: neither src nor a staged copy of it remains.
			require.Equal(t, want, readTree(t, dir))
		})
	}

	t.Run("read-only subdirectory is removed with the source", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"src/sub/f.txt": "f"})
		require.NoError(t, os.Chmod(filepath.Join(dir, "src", "sub"), 0o555))
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "dst", "sub"), 0o755) })

		require.NoError(t, moveEntry(crossDevice, openTestRoot(t, dir), filepath.Join(dir, "src"), filepath.Join(dir, "dst")))
		require.Equal(t, map[string]string{"dst/": "", "dst/sub/": "", "dst/sub/f.txt": "f"}, readTree(t, dir))
	})

	t.Run("failed copy restores the source", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"src/f.txt": "f"})
		require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "src", "pipe"), 0o644))
		want := readTree(t, dir)

		err := moveEntry(crossDevice, openTestRoot(t, dir), filepath.Join(dir, "src"), filepath.Join(dir, "dst"))
		require.ErrorIs(t, err, errUnsupportedFileType)
		require.Equal(t, want, readTree(t, dir))
	})

	t.Run("other rename errors are returned", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"src": "data"})
		src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")

		err := moveEntry(func(string, string) error { return syscall.EACCES }, openTestRoot(t, dir), src, dst)
		require.ErrorIs(t, err, syscall.EACCES)
		_, err = os.Lstat(dst)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestCopyEntryFailureRemovesReadOnlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"src/sub/f.txt": "f", "dst/keep": "k"})
	sub := filepath.Join(dir, "src", "sub")
	require.NoError(t, os.Chmod(sub, 0o555))
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	want := readTree(t, dir)

	// The copy cannot replace the non-empty dst, so its temporary tree goes.
	require.Error(t, copyEntry(openTestRoot(t, dir), filepath.Join(dir, "src"), filepath.Join(dir, "dst")))
	require.Equal(t, want, readTree(t, dir), "no temporary copy may be left behind")
}

func TestStageEntryKeepsReplacementSource(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"file":      {"src": "old"},
		"directory": {"src/f": "old"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, files)
			src := filepath.Join(dir, "src")

			staged, err := stageEntry(src)
			require.NoError(t, err)
			require.Equal(t, dir, filepath.Dir(staged))
			_, err = os.Lstat(src)
			require.ErrorIs(t, err, os.ErrNotExist)

			// A new file at src is not the staged entry, so removing the
			// staged entry leaves it alone.
			writeFiles(t, dir, map[string]string{"src": "new"})
			require.NoError(t, os.RemoveAll(staged))
			require.Equal(t, "new", readFile(t, src))
		})
	}
}

func TestMoveFilesystemRootRefused(t *testing.T) {
	handler := NewRESTServer("/", "SANDBOX_", logr.Discard()).Handler()
	// Only :move is probed. If the guard regresses, rename(2) of "/" fails
	// harmlessly, whereas a :copy would start copying the whole filesystem.
	rec := doAction(handler, actionMove, "", url.Values{"destination": {filepath.Join(t.TempDir(), "x")}})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "INVALID_ARGUMENT", decodeError(t, rec).Code)
}

type tarEntry struct {
	hdr  tar.Header
	body string
}

func tarFile(name, body string, mode int64) tarEntry {
	return tarEntry{tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode}, body}
}

func tarDir(name string, mode int64) tarEntry {
	return tarEntry{tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: mode}, ""}
}

func tarSymlink(name, target string) tarEntry {
	return tarEntry{tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}, ""}
}

func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := e.hdr
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(&hdr))
		_, err := tw.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(data)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func TestExtractTar(t *testing.T) {
	archive := buildTar(t,
		tarDir("d/", 0o755),
		tarFile("d/a.txt", "A", 0o644),
		tarFile("d/run.sh", "#!/bin/sh", 0o755),
		tarFile("top.txt", "T", 0o600),
		// No directory entries for the parents: extraction creates them.
		tarFile("x/y/z.txt", "Z", 0o644),
		tarSymlink("d/ln", "a.txt"),
	)
	for name, body := range map[string][]byte{"plain": archive, "gzip": gzipped(t, archive)} {
		t.Run(name, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)

			rec := doRequest(handler, http.MethodPut, "/v1/files/dest?extract=tar", body, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

			for path, want := range map[string]string{"d/a.txt": "A", "d/run.sh": "#!/bin/sh", "top.txt": "T", "x/y/z.txt": "Z"} {
				got, err := os.ReadFile(filepath.Join(root, "dest", path))
				require.NoError(t, err)
				require.Equal(t, want, string(got))
			}
			for path, want := range map[string]os.FileMode{"d/run.sh": 0o755, "top.txt": 0o600, "d": 0o755} {
				info, err := os.Stat(filepath.Join(root, "dest", path))
				require.NoError(t, err)
				require.Equal(t, want, info.Mode().Perm(), path)
			}
			link, err := os.Readlink(filepath.Join(root, "dest", "d", "ln"))
			require.NoError(t, err)
			require.Equal(t, "a.txt", link)
		})
	}
}

func TestExtractTarMultipart(t *testing.T) {
	_, handler, root := newRESTFixture(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "a.tar")
	require.NoError(t, err)
	_, err = part.Write(buildTar(t, tarFile("a.txt", "A", 0o644)))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	header := http.Header{"Content-Type": []string{mw.FormDataContentType()}}
	rec := doRequest(handler, http.MethodPut, "/v1/files/dest?extract=tar", buf.Bytes(), header)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got, err := os.ReadFile(filepath.Join(root, "dest", "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "A", string(got))
}

func TestExtractTarMergesAndReplacesInPlace(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dest"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dest", "a.txt"), []byte("old"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dest", "keep.txt"), []byte("keep"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "victim"), []byte("victim"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(root, "victim"), filepath.Join(root, "dest", "link")))

	archive := buildTar(t, tarFile("a.txt", "new", 0o644), tarFile("link", "new", 0o644))
	rec := doRequest(handler, http.MethodPut, "/v1/files/dest?extract=tar", archive, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	for path, want := range map[string]string{"dest/a.txt": "new", "dest/keep.txt": "keep", "dest/link": "new", "victim": "victim"} {
		got, err := os.ReadFile(filepath.Join(root, path))
		require.NoError(t, err)
		require.Equal(t, want, string(got), path)
	}
	info, err := os.Lstat(filepath.Join(root, "dest", "link"))
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular(), "the symlink is replaced, not followed")
}

func TestExtractTarSkipsGlobalHeaderAndRoot(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// `git archive` starts with a pax global header, and many tools add "./".
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "abc"},
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./f.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}))
	_, err := tw.Write([]byte("f"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	_, handler, root := newRESTFixture(t)
	rec := doRequest(handler, http.MethodPut, "/v1/files/dest?extract=tar", buf.Bytes(), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	entries, err := os.ReadDir(filepath.Join(root, "dest"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "f.txt", entries[0].Name())
}

func TestExtractTarRejections(t *testing.T) {
	valid := func(t *testing.T) []byte {
		return buildTar(t, tarFile("a.txt", "A", 0o644), tarFile("b.txt", "BBBBBBBBBB", 0o644))
	}
	tests := []struct {
		name       string
		setup      func(t *testing.T, root string)
		query      string
		header     http.Header
		archive    func(t *testing.T, outside string) []byte
		wantStatus int
		wantCode   string
	}{
		{
			name:       "parent traversal",
			archive:    func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("../evil", "x", 0o644)) },
			wantStatus: http.StatusForbidden, wantCode: "PERMISSION_DENIED",
		},
		{
			name:       "traversal inside a path",
			archive:    func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("a/../../evil", "x", 0o644)) },
			wantStatus: http.StatusForbidden, wantCode: "PERMISSION_DENIED",
		},
		{
			name: "absolute path",
			archive: func(t *testing.T, outside string) []byte {
				return buildTar(t, tarFile(filepath.Join(outside, "evil"), "x", 0o644))
			},
			wantStatus: http.StatusForbidden, wantCode: "PERMISSION_DENIED",
		},
		{
			name: "write through a symlink planted by an earlier entry",
			archive: func(t *testing.T, outside string) []byte {
				return buildTar(t, tarSymlink("out", outside), tarFile("out/evil", "x", 0o644))
			},
			wantStatus: http.StatusForbidden, wantCode: "PERMISSION_DENIED",
		},
		{
			name: "write through a symlink to a sibling directory in the root",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "other"), 0o755))
			},
			archive: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarSymlink("ln", "../other"), tarFile("ln/evil", "x", 0o644))
			},
			wantStatus: http.StatusForbidden, wantCode: "PERMISSION_DENIED",
		},
		{
			name: "hard link",
			archive: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "/etc/passwd", Mode: 0o644}})
			},
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
		{
			name: "fifo",
			archive: func(t *testing.T, _ string) []byte {
				return buildTar(t, tarEntry{hdr: tar.Header{Name: "p", Typeflag: tar.TypeFifo, Mode: 0o644}})
			},
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
		{
			name: "file over directory",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "dest", "d"), 0o755))
			},
			archive:    func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("d", "x", 0o644)) },
			wantStatus: http.StatusConflict, wantCode: "CONFLICT",
		},
		{
			name: "directory over file",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "dest"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "dest", "f"), []byte("x"), 0o644))
			},
			archive:    func(t *testing.T, _ string) []byte { return buildTar(t, tarDir("f/", 0o755)) },
			wantStatus: http.StatusConflict, wantCode: "CONFLICT",
		},
		{
			name: "file below a file",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "dest"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "dest", "f"), []byte("x"), 0o644))
			},
			archive:    func(t *testing.T, _ string) []byte { return buildTar(t, tarFile("f/g", "x", 0o644)) },
			wantStatus: http.StatusConflict, wantCode: "CONFLICT",
		},
		{
			name: "destination is a file",
			setup: func(t *testing.T, root string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "dest"), []byte("x"), 0o644))
			},
			archive:    func(t *testing.T, _ string) []byte { return valid(t) },
			wantStatus: http.StatusConflict, wantCode: "CONFLICT",
		},
		{
			name:       "not a tar",
			archive:    func(*testing.T, string) []byte { return bytes.Repeat([]byte("x"), 1024) },
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
		{
			name:       "bad gzip header",
			archive:    func(*testing.T, string) []byte { return []byte{0x1f, 0x8b, 0xff, 0xff, 0xff} },
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
		{
			name:       "unknown format",
			query:      "?extract=zip",
			archive:    func(t *testing.T, _ string) []byte { return valid(t) },
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
		{
			name:       "with append",
			query:      "?extract=tar&append=true",
			archive:    func(t *testing.T, _ string) []byte { return valid(t) },
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
		{
			name:       "with if-none-match",
			query:      "?extract=tar",
			header:     http.Header{"If-None-Match": {"*"}},
			archive:    func(t *testing.T, _ string) []byte { return valid(t) },
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
		{
			name:       "with type",
			query:      "?extract=tar&type=directory",
			archive:    func(t *testing.T, _ string) []byte { return valid(t) },
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
		{
			name:       "invalid mode",
			query:      "?extract=tar&mode=999",
			archive:    func(t *testing.T, _ string) []byte { return valid(t) },
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ARGUMENT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, handler, root := newRESTFixture(t)
			outside := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, root)
			}
			query := tc.query
			if query == "" {
				query = "?extract=tar"
			}

			rec := doRequest(handler, http.MethodPut, "/v1/files/dest"+query, tc.archive(t, outside), tc.header)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			require.Equal(t, tc.wantCode, decodeError(t, rec).Code)

			require.NoFileExists(t, filepath.Join(root, "evil"))
			require.NoFileExists(t, filepath.Join(root, "other", "evil"))
			require.NoFileExists(t, filepath.Join(filepath.Dir(root), "evil"))
			outsideEntries, err := os.ReadDir(outside)
			require.NoError(t, err)
			require.Empty(t, outsideEntries, "nothing may be written outside the sandbox root")
		})
	}
}

func TestExtractTarLeavesNoTempFileOnTruncatedBody(t *testing.T) {
	_, handler, root := newRESTFixture(t)
	archive := buildTar(t, tarFile("big.txt", string(bytes.Repeat([]byte("x"), 4096)), 0o644))

	rec := doRequest(handler, http.MethodPut, "/v1/files/dest?extract=tar", archive[:1024], nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	entries, err := os.ReadDir(filepath.Join(root, "dest"))
	require.NoError(t, err)
	require.Empty(t, entries, "a failed entry leaves no partial file or temp file")
}

func TestExtractTarCaps(t *testing.T) {
	tests := []struct {
		name    string
		entries int
		bytes   int64
		archive []tarEntry
		// wantWritten lists the files that exist after the request.
		wantWritten []string
		wantStatus  int
	}{
		{
			name: "entries at the cap", entries: 2, bytes: 100,
			archive:     []tarEntry{tarFile("a", "A", 0o644), tarFile("b", "B", 0o644)},
			wantWritten: []string{"a", "b"}, wantStatus: http.StatusNoContent,
		},
		{
			name: "entries over the cap", entries: 2, bytes: 100,
			archive:     []tarEntry{tarFile("a", "A", 0o644), tarDir("d/", 0o755), tarFile("b", "B", 0o644)},
			wantWritten: []string{"a"}, wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name: "bytes at the cap", entries: 10, bytes: 8,
			archive:     []tarEntry{tarFile("a", "AAAA", 0o644), tarFile("b", "BBBB", 0o644)},
			wantWritten: []string{"a", "b"}, wantStatus: http.StatusNoContent,
		},
		{
			name: "bytes over the cap", entries: 10, bytes: 8,
			archive:     []tarEntry{tarFile("a", "AAAA", 0o644), tarFile("b", "BBBBB", 0o644)},
			wantWritten: []string{"a"}, wantStatus: http.StatusRequestEntityTooLarge,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, handler, root := newRESTFixture(t)
			s.maxExtractEntries = tc.entries
			s.maxExtractBytes = tc.bytes
			archive := gzipped(t, buildTar(t, tc.archive...))

			rec := doRequest(handler, http.MethodPut, "/v1/files/dest?extract=tar", archive, nil)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			if tc.wantStatus != http.StatusNoContent {
				require.Equal(t, "RESOURCE_EXHAUSTED", decodeError(t, rec).Code)
			}

			var written []string
			dirEntries, err := os.ReadDir(filepath.Join(root, "dest"))
			require.NoError(t, err)
			for _, e := range dirEntries {
				if !e.IsDir() {
					written = append(written, e.Name())
				}
			}
			require.Equal(t, tc.wantWritten, written)
		})
	}
}
