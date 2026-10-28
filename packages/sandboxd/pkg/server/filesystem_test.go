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
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
