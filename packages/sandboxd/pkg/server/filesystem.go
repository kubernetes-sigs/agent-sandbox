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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/agent-sandbox/packages/sandboxd/pkg/pathutil"
)

// Wire types live in filesystem_types.go; the /v1/metadata handler lives in
// metadata.go.

var modePattern = regexp.MustCompile(`^0[0-7]{3}$`)

// Values of FileEntry.Type, also accepted by the "type" query on GET and HEAD.
const (
	entryTypeFile      = "file"
	entryTypeDirectory = "directory"
)

// RESTServer implements the sandboxd Filesystem & Runtime REST API on top of
// net/http. All file paths are confined to rootDir via pathutil.SanitizePath.
type RESTServer struct {
	rootDir           string
	resolvedRoot      string
	metadataEnvPrefix string
	startTime         time.Time
	ready             atomic.Bool
	log               logr.Logger

	// environ is swappable for tests; defaults to os.Environ.
	environ func() []string
	// metadataEnv returns the filtered /v1/metadata env map, computed once
	// on first use: the daemon's environment is immutable after start.
	metadataEnv func() map[string]string
}

// NewRESTServer builds a RESTServer rooted at rootDir. rootDir must be
// non-empty. Only environment variables carrying metadataEnvPrefix are exposed
// on /v1/metadata. The server starts ready; SetReady(false) flips /v1/health
// to 503.
func NewRESTServer(rootDir, metadataEnvPrefix string, log logr.Logger) *RESTServer {
	if metadataEnvPrefix == "" {
		metadataEnvPrefix = "SANDBOX_"
	}
	resolvedRoot, err := pathutil.SanitizePath(rootDir, ".")
	if err != nil {
		resolvedRoot, _ = filepath.Abs(filepath.Clean(rootDir))
	}
	s := &RESTServer{
		rootDir:           rootDir,
		resolvedRoot:      resolvedRoot,
		metadataEnvPrefix: metadataEnvPrefix,
		startTime:         time.Now(),
		log:               log,
		environ:           os.Environ,
	}
	s.ready.Store(true)
	s.metadataEnv = sync.OnceValue(s.buildMetadataEnv)
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

// boolQuery reads an optional boolean query parameter. It writes a 400 and
// returns ok=false when the value does not parse.
func (s *RESTServer) boolQuery(w http.ResponseWriter, r *http.Request, name string) (value, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, true
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("%s %q must be a boolean", name, raw))
		return false, false
	}
	return parsed, true
}

// typeQuery reads the optional "type" query parameter. It writes a 400 and
// returns ok=false when the value is neither "file" nor "directory".
func (s *RESTServer) typeQuery(w http.ResponseWriter, r *http.Request) (value string, ok bool) {
	query := r.URL.Query()
	value = query.Get("type")
	if query.Has("type") && value != entryTypeFile && value != entryTypeDirectory {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("type %q must be %q or %q", value, entryTypeFile, entryTypeDirectory))
		return "", false
	}
	return value, true
}

// modeQuery reads the optional "mode" query parameter, returning def when it
// is absent. It writes a 400 and returns ok=false when the value is invalid.
func (s *RESTServer) modeQuery(w http.ResponseWriter, r *http.Request, def os.FileMode) (os.FileMode, bool) {
	raw := r.URL.Query().Get("mode")
	if raw == "" {
		return def, true
	}
	if !modePattern.MatchString(raw) {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("mode %q must match ^0[0-7]{3}$", raw))
		return 0, false
	}
	parsed, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return 0, false
	}
	return os.FileMode(parsed), true
}

func (s *RESTServer) handleGetFile(w http.ResponseWriter, r *http.Request) {
	wantType, ok := s.typeQuery(w, r)
	if !ok {
		return
	}

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

	if msg := typeMismatch(wantType, info.IsDir()); msg != "" {
		s.writeError(w, http.StatusConflict, "CONFLICT", msg)
		return
	}

	if info.IsDir() {
		s.serveDirectoryListing(w, r, target)
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
	// ServeContent streams via seek + chunked io.Copy — the file is never
	// fully buffered in memory, so downloads are not size-capped. It also
	// handles HEAD, Content-Length, and Range requests, and keeps the
	// Content-Type set above.
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// typeMismatch returns why the path disagrees with the "type" query, or "".
func typeMismatch(want string, isDir bool) string {
	switch {
	case want == entryTypeFile && isDir:
		return "path is a directory, not a file"
	case want == entryTypeDirectory && !isDir:
		return "path is a file, not a directory"
	}
	return ""
}

func (s *RESTServer) serveDirectoryListing(w http.ResponseWriter, r *http.Request, target string) {
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
			// Resolve the link's type through SanitizePath so links escaping
			// the sandbox root are omitted from the listing. Broken links are also omitted.
			resolved, sanErr := pathutil.SanitizePath(s.rootDir, filepath.Join(r.PathValue("path"), entry.Name()))
			if sanErr != nil {
				continue
			}
			info, err = os.Stat(resolved)
			if err != nil {
				continue
			}
		}
		entryType := entryTypeFile
		if info.IsDir() {
			entryType = entryTypeDirectory
		}
		entries = append(entries, FileEntry{
			Name:       entry.Name(),
			Size:       info.Size(),
			Type:       entryType,
			ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
			Mode:       fmt.Sprintf("%04o", info.Mode().Perm()),
		})
	}

	relPath := r.PathValue("path")
	if !strings.HasPrefix(relPath, "/") {
		relPath = "/" + relPath
	}
	s.writeJSON(w, http.StatusOK, DirectoryListing{Path: relPath, Entries: entries})
}

func (s *RESTServer) handlePutFile(w http.ResponseWriter, r *http.Request) {
	putType, ok := s.typeQuery(w, r)
	if !ok {
		return
	}
	makeDirectory := putType == entryTypeDirectory

	defaultMode := os.FileMode(0o644)
	if makeDirectory {
		defaultMode = 0o755
	}
	mode, ok := s.modeQuery(w, r, defaultMode)
	if !ok {
		return
	}

	appendMode, ok := s.boolQuery(w, r, "append")
	if !ok {
		return
	}

	target, ok := s.resolvePath(w, r)
	if !ok {
		return
	}

	if makeDirectory {
		if appendMode {
			s.writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				"append cannot be used with type=directory")
			return
		}
		s.putDirectory(w, r, target, mode)
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

	write := atomicWrite
	if appendMode {
		write = appendWrite
	}
	if err := write(target, body, mode); err != nil {
		if os.IsPermission(err) {
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		} else {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// putDirectory handles PUT with type=directory. It ignores the request body.
func (s *RESTServer) putDirectory(w http.ResponseWriter, r *http.Request, target string, mode os.FileMode) {
	parents, ok := s.boolQuery(w, r, "parents")
	if !ok {
		return
	}

	if err := makeDirectory(target, mode, parents); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			s.writeError(w, http.StatusNotFound, "NOT_FOUND",
				"parent directory does not exist; pass parents=true to create it")
		case errors.Is(err, os.ErrExist):
			s.writeError(w, http.StatusConflict, "CONFLICT", "path exists and is not a directory")
		case os.IsPermission(err):
			s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission denied")
		default:
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// makeDirectory creates the directory target, and its missing parents when
// parents is true. It succeeds when target is already a directory, so retries
// are safe, and applies mode only to a directory it creates.
func makeDirectory(target string, mode os.FileMode, parents bool) error {
	if parents {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("failed to create parent directory: %w", err)
		}
	}

	err := os.Mkdir(target, mode)
	if errors.Is(err, os.ErrExist) {
		if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Mkdir applies the umask; chmod so a new directory gets exactly mode, as
	// atomicWrite does for files.
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("failed to set directory mode: %w", err)
	}
	return nil
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
// strategy. Parent directories are created automatically.
//
// Concurrency: handlers run in parallel (one goroutine per request, no
// cross-request locking). Each successful rename(2) publishes a complete file
// atomically, so an already-open reader sees either the previous complete file
// or the new one. A concurrent delete can remove the temporary file or its
// parent directory before rename, in which case the write fails.
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

// appendWrite streams src to the end of target. It creates target, and its
// parent directories, when missing, and applies mode only to a file it creates.
// Unlike atomicWrite, readers can see a partial append, and concurrent appends
// can interleave between writes.
func appendWrite(target string, src io.Reader, mode os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	// O_EXCL tells us whether this request created the file.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, mode)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	}
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("failed to close file: %w", cerr)
		}
	}()

	// OpenFile applies the umask; chmod so a new file gets exactly mode, as
	// atomicWrite does.
	if created {
		if err = f.Chmod(mode); err != nil {
			return fmt.Errorf("failed to set file mode: %w", err)
		}
	}
	if _, err = io.Copy(f, src); err != nil {
		return fmt.Errorf("failed to append file contents: %w", err)
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

	recursive, ok := s.boolQuery(w, r, "recursive")
	if !ok {
		return
	}

	// With --root-dir=/ the root is the whole filesystem; clearing it would
	// remove the daemon, the runtime and every mount.
	if target == string(filepath.Separator) {
		s.writeError(w, http.StatusForbidden, "PERMISSION_DENIED",
			"refusing to delete the filesystem root")
		return
	}

	if target == s.resolvedRoot {
		if !recursive {
			s.writeError(w, http.StatusConflict, "CONFLICT",
				"cannot delete the sandbox root; pass recursive=true to clear its contents")
			return
		}
		// For the sandbox root directory itself, clear its contents rather
		// than deleting the volume mount entity.
		dirEntries, err := os.ReadDir(target)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		for _, entry := range dirEntries {
			if err := os.RemoveAll(filepath.Join(target, entry.Name())); err != nil {
				s.writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
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
