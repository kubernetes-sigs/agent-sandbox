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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
)

// Wire types matching packages/sandboxd/spec/filesystem/v1/filesystem.yaml.
// Hand-written rather than generated: the repo carries no OpenAPI codegen
// toolchain and the surface is small; conformance is pinned by tests.

// FileEntry is one row of a DirectoryListing.
type FileEntry struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Type       string `json:"type"` // "file" | "directory"
	ModifiedAt string `json:"modified_at"`
	Mode       string `json:"mode,omitempty"` // octal, e.g. "0644"
}

// DirectoryListing is returned by GET /v1/files/{path} for directories.
type DirectoryListing struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	Status        string `json:"status"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// MetadataResponse is returned by GET /v1/metadata.
type MetadataResponse struct {
	Env map[string]string `json:"env"`
}

// APIError is the error body shared by all non-2xx responses.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var modePattern = regexp.MustCompile(`^0[0-7]{3}$`)

// sensitiveEnvMarkers guards /v1/metadata against accidentally exposing
// credentials even when they match the allowlist prefix: untrusted agent
// code can query this endpoint over loopback.
var sensitiveEnvMarkers = []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "KEY"}

// RESTServer implements the sandboxd Filesystem & Runtime REST API on top of
// net/http. All file paths are confined to rootDir via pathutil.SanitizePath.
type RESTServer struct {
	rootDir           string
	metadataEnvPrefix string
	startTime         time.Time
	ready             atomic.Bool
	log               logr.Logger

	// environ is swappable for tests; defaults to os.Environ.
	environ func() []string
}

// NewRESTServer builds a RESTServer rooted at rootDir. Only environment
// variables carrying metadataEnvPrefix are exposed on /v1/metadata.
// The server starts ready; SetReady(false) flips /v1/health to 503.
func NewRESTServer(rootDir, metadataEnvPrefix string, log logr.Logger) *RESTServer {
	if rootDir == "" {
		rootDir = "/"
	}
	s := &RESTServer{
		rootDir:           rootDir,
		metadataEnvPrefix: metadataEnvPrefix,
		startTime:         time.Now(),
		log:               log,
		environ:           os.Environ,
	}
	s.ready.Store(true)
	return s
}

// SetReady toggles the /v1/health response between 200 and 503. Flipped to
// false at the start of graceful shutdown so Kubernetes stops routing.
func (s *RESTServer) SetReady(ready bool) {
	s.ready.Store(ready)
}

// Handler returns the fully routed http.Handler for the REST API.
func (s *RESTServer) Handler() http.Handler {
	mux := http.NewServeMux()
	// A "GET" pattern also serves HEAD with the body suppressed, giving the
	// KEP's existence probe (HEAD /v1/files/{path}) for free.
	mux.HandleFunc("GET /v1/files/{path...}", s.handleGetFile)
	mux.HandleFunc("PUT /v1/files/{path...}", s.handlePutFile)
	mux.HandleFunc("DELETE /v1/files/{path...}", s.handleDeleteFile)
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/metadata", s.handleMetadata)
	return mux
}

func (s *RESTServer) writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.V(4).Info("failed to encode response body", "error", err)
	}
}

func (s *RESTServer) writeError(w http.ResponseWriter, httpCode int, code, message string) {
	s.writeJSON(w, httpCode, APIError{Code: code, Message: message})
}

// resolvePath sanitizes the {path...} wildcard against the sandbox root,
// writing the spec-mandated error response on failure.
func (s *RESTServer) resolvePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	target, err := pathutil.SanitizePath(s.rootDir, r.PathValue("path"))
	if err != nil {
		if errors.Is(err, pathutil.ErrPathEscapes) {
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED",
				"path traversal outside sandbox root is forbidden")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return "", false
	}
	return target, true
}

func (s *RESTServer) handleGetFile(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolvePath(w, r)
	if !ok {
		return
	}

	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "NOT_FOUND", "path does not exist within the sandbox")
		} else if os.IsPermission(err) {
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	if info.IsDir() {
		s.serveDirectoryListing(w, target)
		return
	}

	f, err := os.Open(target)
	if err != nil {
		if os.IsPermission(err) {
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent handles HEAD, Content-Length, and Range requests, and
	// keeps the Content-Type we set above.
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (s *RESTServer) serveDirectoryListing(w http.ResponseWriter, target string) {
	dirEntries, err := os.ReadDir(target) // sorted by name
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	entries := make([]FileEntry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		info, err := entry.Info()
		if err != nil {
			continue // entry vanished between ReadDir and Info
		}
		if entry.Type()&os.ModeSymlink != 0 {
			// The wire format only knows file|directory: resolve the link
			// target's type. Broken links are omitted from the listing.
			info, err = os.Stat(filepath.Join(target, entry.Name()))
			if err != nil {
				continue
			}
		}
		entryType := "file"
		if info.IsDir() {
			entryType = "directory"
		}
		entries = append(entries, FileEntry{
			Name:       entry.Name(),
			Size:       info.Size(),
			Type:       entryType,
			ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
			Mode:       fmt.Sprintf("%04o", info.Mode().Perm()),
		})
	}

	s.writeJSON(w, http.StatusOK, DirectoryListing{Path: target, Entries: entries})
}

func (s *RESTServer) handlePutFile(w http.ResponseWriter, r *http.Request) {
	mode := os.FileMode(0o644)
	if modeParam := r.URL.Query().Get("mode"); modeParam != "" {
		if !modePattern.MatchString(modeParam) {
			s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				fmt.Sprintf("mode %q must match ^0[0-7]{3}$", modeParam))
			return
		}
		parsed, err := strconv.ParseUint(modeParam, 8, 32)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
			return
		}
		mode = os.FileMode(parsed)
	}

	target, ok := s.resolvePath(w, r)
	if !ok {
		return
	}
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "target path is a directory")
		return
	}

	body, cleanup, err := requestFileBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	defer cleanup()

	if err := atomicWrite(target, body, mode); err != nil {
		if os.IsPermission(err) {
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// requestFileBody returns the file payload reader for a PUT request: the raw
// body, or the required "file" part of a multipart/form-data upload.
func requestFileBody(r *http.Request) (io.Reader, func(), error) {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if contentType != "" && err == nil && mediaType == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			return nil, nil, fmt.Errorf("invalid multipart body: %w", err)
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return nil, nil, fmt.Errorf("multipart body is missing the required %q part", "file")
			}
			if err != nil {
				return nil, nil, fmt.Errorf("invalid multipart body: %w", err)
			}
			if part.FormName() == "file" {
				return part, func() { _ = part.Close() }, nil
			}
			_ = part.Close()
		}
	}
	return r.Body, func() {}, nil
}

// atomicWrite streams src into target using the temp-file-then-rename
// strategy so concurrent readers never observe a partial file. Parent
// directories are created automatically.
func atomicWrite(target string, src io.Reader, mode os.FileMode) (err error) {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, ".sandboxd-tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	// Remove the temp file on any failure, including client disconnects
	// mid-upload.
	defer func() {
		if err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
		}
	}()

	if err = tmpFile.Chmod(mode); err != nil {
		return fmt.Errorf("failed to set file mode: %w", err)
	}
	if _, err = io.Copy(tmpFile, src); err != nil {
		return fmt.Errorf("failed to write file contents: %w", err)
	}
	if err = tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}
	if err = tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}
	if err = os.Rename(tmpFile.Name(), target); err != nil {
		return fmt.Errorf("failed to move file into place: %w", err)
	}
	return nil
}

func (s *RESTServer) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolvePath(w, r)
	if !ok {
		return
	}

	if _, err := os.Lstat(target); err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, http.StatusNotFound, "NOT_FOUND", "path does not exist within the sandbox")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	recursive := false
	if recParam := r.URL.Query().Get("recursive"); recParam != "" {
		parsed, err := strconv.ParseBool(recParam)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				fmt.Sprintf("recursive %q must be a boolean", recParam))
			return
		}
		recursive = parsed
	}

	var removeErr error
	if recursive {
		removeErr = os.RemoveAll(target)
	} else {
		removeErr = os.Remove(target)
	}
	if removeErr != nil {
		switch {
		case errors.Is(removeErr, syscall.ENOTEMPTY) || errors.Is(removeErr, syscall.EEXIST):
			s.writeError(w, http.StatusConflict, "CONFLICT",
				"directory is not empty; pass recursive=true to remove it")
		case os.IsPermission(removeErr):
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		default:
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", removeErr.Error())
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *RESTServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		s.writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "sandboxd is shutting down")
		return
	}
	s.writeJSON(w, http.StatusOK, HealthResponse{
		Status:        "ok",
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
	})
}

func (s *RESTServer) handleMetadata(w http.ResponseWriter, _ *http.Request) {
	env := map[string]string{}
	for _, kv := range s.environ() {
		name, value, found := strings.Cut(kv, "=")
		if !found || !strings.HasPrefix(name, s.metadataEnvPrefix) {
			continue
		}
		if isSensitiveEnvName(name) {
			continue
		}
		env[name] = value
	}
	s.writeJSON(w, http.StatusOK, MetadataResponse{Env: env})
}

// isSensitiveEnvName reports whether an environment variable name looks like
// it carries a credential. The spec forbids serving orchestrator credentials,
// API tokens, or cloud IAM keys on /v1/metadata.
func isSensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range sensitiveEnvMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}
